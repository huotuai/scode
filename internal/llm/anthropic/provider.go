package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"scode/internal/llm"
)

// Provider streams assistant turns over the Anthropic Messages API.
// BaseURL redirection (GLM's Anthropic-compatible endpoint, relays) works
// by construction: only the host changes.
type Provider struct {
	Key     string
	BaseURL string
	HTTP    *http.Client
}

func New(key, baseURL string) *Provider {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Provider{Key: key, BaseURL: baseURL, HTTP: http.DefaultClient}
}

func (p *Provider) Name() string { return "anthropic" }

func (p *Provider) Caps() llm.Capabilities {
	return llm.Capabilities{
		CacheBreakpoints:       true,
		LongCacheRetention:     true, // per model; request builder re-checks
		MidConversationSystem:  false,
		StreamingToolArguments: true,
		// Tool additions anchor mid-conversation in a later revision; the
		// current path sends the current set top-level, prefix-stable.
		ToolAdditions: false,
	}
}

// maxAttempts caps pre-stream retries. Once streaming has begun, failures
// are terminal events — higher-level retry policy is the agent loop's call.
const maxAttempts = 4

// streamTimeout bounds a turn when the caller set no deadline.
const streamTimeout = 10 * time.Minute

func (p *Provider) resolveKey(opts llm.StreamOptions) string {
	if opts.APIKey != "" {
		return opts.APIKey
	}
	if p.Key != "" {
		return p.Key
	}
	return os.Getenv("ANTHROPIC_API_KEY")
}

func (p *Provider) Stream(ctx context.Context, model llm.Model, t *llm.Transcript, opts llm.StreamOptions) (<-chan llm.Event, error) {
	key := p.resolveKey(opts)
	if key == "" {
		return nil, fmt.Errorf("anthropic: no API key (set ANTHROPIC_API_KEY or pass StreamOptions.APIKey)")
	}
	req, err := BuildRequest(model, t, opts)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	out := make(chan llm.Event, 64)
	go func() {
		defer close(out)
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, streamTimeout)
			defer cancel()
		}
		resp, err := p.post(ctx, key, body)
		if err != nil {
			emitError(out, model.ID, err.Error())
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			msg := readErrorBody(resp.Body)
			emitError(out, model.ID, fmt.Sprintf("anthropic: HTTP %d: %s", resp.StatusCode, msg))
			return
		}
		pump(ctx, resp.Body, model.ID, out)
	}()
	return out, nil
}

// post sends the request with retry on transient failures via the shared
// helper (network errors, 408/429/5xx, Retry-After honored). A 200 with an
// SSE body is success; other statuses return for one-time surfacing.
func (p *Provider) post(ctx context.Context, key string, body []byte) (*http.Response, error) {
	return llm.PostStreamWithRetry(ctx, p.HTTP, p.BaseURL+"/v1/messages", map[string][]string{
		"content-type":      {"application/json"},
		"x-api-key":         {key},
		"anthropic-version": {APIVersion},
	}, body, maxAttempts)
}

// pump reads the SSE body through the assembler into the event channel.
// Caller-owned body close on cancellation unblocks the reader; that
// surfaces as an aborted error event.
func pump(ctx context.Context, r io.Reader, model string, out chan<- llm.Event) {
	asm := newAssembler(model)
	err := llm.ReadSSE(r, func(ev llm.SSEEvent) error {
		events, terminal, err := asm.handle(ev.Name, ev.Data)
		for _, e := range events {
			select {
			case out <- e:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err != nil {
			return err
		}
		if terminal {
			return io.EOF // stop reading; message_stop / error reached
		}
		return nil
	})
	if err != nil && err != io.EOF {
		msg := asm.snapshot()
		msg.StopReason = llm.StopError
		if ctx.Err() != nil {
			msg.StopReason = llm.StopAborted
			msg.Error = "aborted by caller"
		} else {
			msg.Error = err.Error()
		}
		out <- llm.Event{Type: llm.EventError, Message: &msg, Reason: msg.StopReason, Err: err}
	}
}

func emitError(out chan<- llm.Event, model, text string) {
	msg := llm.Message{
		Role:       llm.RoleAssistant,
		Model:      model,
		Provider:   "anthropic",
		StopReason: llm.StopError,
		Error:      text,
	}
	out <- llm.Event{Type: llm.EventError, Message: &msg, Reason: llm.StopError, Err: fmt.Errorf("%s", text)}
}

func readErrorBody(r io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(r, 64<<10))
	if err != nil || len(b) == 0 {
		return "(no body)"
	}
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
		return fmt.Sprintf("%s: %s", e.Error.Type, e.Error.Message)
	}
	return string(b)
}
