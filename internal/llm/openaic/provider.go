package openaic

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

// Provider streams assistant turns over any OpenAI-compatible
// chat/completions endpoint (OpenAI, GLM, DeepSeek, OpenRouter, relays).
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

func (p *Provider) Name() string { return "openai-compat" }

func (p *Provider) Caps() llm.Capabilities {
	return llm.Capabilities{
		// Caching is implicit prefix matching; there are no breakpoints to
		// place and no TTL to choose, but prefix stability still pays off.
		CacheBreakpoints:       false,
		LongCacheRetention:     false,
		MidConversationSystem:  true,
		StreamingToolArguments: true,
		ToolAdditions:          false,
	}
}

const maxAttempts = 4

const streamTimeout = 10 * time.Minute

func (p *Provider) resolveKey(opts llm.StreamOptions) string {
	if opts.APIKey != "" {
		return opts.APIKey
	}
	if p.Key != "" {
		return p.Key
	}
	for _, env := range []string{"OPENAI_API_KEY", "OPENAI_COMPAT_API_KEY"} {
		if v := os.Getenv(env); v != "" {
			return v
		}
	}
	return ""
}

func (p *Provider) Stream(ctx context.Context, model llm.Model, t *llm.Transcript, opts llm.StreamOptions) (<-chan llm.Event, error) {
	key := p.resolveKey(opts)
	if key == "" {
		return nil, fmt.Errorf("openai-compat: no API key (set OPENAI_API_KEY or pass StreamOptions.APIKey)")
	}
	if opts.BaseURL == "" {
		opts.BaseURL = p.BaseURL // request building keys off the real endpoint
	}
	req, err := BuildRequest(model, t, opts)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if os.Getenv("SCODE_DEBUG_REQ") == "1" {
		fmt.Fprintf(os.Stderr, "[scode] request: %s\n", body)
	}

	out := make(chan llm.Event, 64)
	go func() {
		defer close(out)
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, streamTimeout)
			defer cancel()
		}
		resp, err := llm.PostStreamWithRetry(ctx, p.HTTP, p.BaseURL+"/chat/completions", map[string][]string{
			"content-type":  {"application/json"},
			"accept":        {"text/event-stream"},
			"authorization": {"Bearer " + key},
		}, body, maxAttempts)
		if err != nil {
			p.emitError(out, model.ID, err.Error())
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			p.emitError(out, model.ID, fmt.Sprintf("openai-compat: HTTP %d: %s", resp.StatusCode, readErrorBody(resp.Body)))
			return
		}
		p.pump(ctx, resp.Body, model.ID, out)
	}()
	return out, nil
}

func (p *Provider) pump(ctx context.Context, r io.Reader, model string, out chan<- llm.Event) {
	asm := newAssembler(model)
	debug := os.Getenv("SCODE_DEBUG_SSE") == "1"
	err := llm.ReadSSE(r, func(ev llm.SSEEvent) error {
		if debug {
			fmt.Fprintf(os.Stderr, "[scode] sse: %s\n", ev.Data)
		}
		events, terminal, err := asm.handle(ev.Data) // chat chunks arrive as unnamed data events
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
			return io.EOF
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

func (p *Provider) emitError(out chan<- llm.Event, model, text string) {
	msg := llm.Message{
		Role:       llm.RoleAssistant,
		Model:      model,
		Provider:   "openai-compat",
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
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &e) == nil {
		if e.Error.Message != "" {
			return fmt.Sprintf("%s: %s", e.Error.Type, e.Error.Message)
		}
		if e.Message != "" {
			return e.Message
		}
	}
	return string(b)
}
