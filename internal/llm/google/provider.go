// Package google implements the Google Generative AI (Gemini) adapter
// (pi's google-generative-ai.ts + google-shared.ts): transcript →
// generateContent request, SSE stream → llm.Event assembly.
//
// scode talks plain HTTP+SSE where pi uses the @google/genai SDK; the
// wire shape is identical: POST {base}/models/{model}:streamGenerateContent
// with alt=sse, the key in the x-goog-api-key header.
package google

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"scode/internal/llm"
)

// Provider streams assistant turns over the Gemini generateContent API.
type Provider struct {
	Key     string
	BaseURL string
	HTTP    *http.Client
	// Transport carries pi's settings.retry.provider knobs
	// (timeout, attempts, backoff cap).
	Transport llm.TransportConfig
}

// DefaultBaseURL is pi's google provider baseUrl.
const DefaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

func New(key, baseURL string) *Provider {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Provider{Key: key, BaseURL: baseURL, HTTP: http.DefaultClient}
}

func (p *Provider) Name() string { return "google" }

func (p *Provider) Caps() llm.Capabilities {
	// No cache breakpoints, no mid-conversation system messages (folded
	// into systemInstruction), and function calls arrive complete in one
	// part (no streaming argument fragments).
	return llm.Capabilities{}
}

func (p *Provider) transport() llm.TransportConfig { return p.Transport }

func (p *Provider) resolveKey(opts llm.StreamOptions) string {
	if opts.APIKey != "" {
		return opts.APIKey
	}
	if p.Key != "" {
		return p.Key
	}
	return os.Getenv("GEMINI_API_KEY")
}

func (p *Provider) Stream(ctx context.Context, model llm.Model, t *llm.Transcript, opts llm.StreamOptions) (<-chan llm.Event, error) {
	key := p.resolveKey(opts)
	if key == "" {
		return nil, fmt.Errorf("google: no API key (set GEMINI_API_KEY or pass StreamOptions.APIKey)")
	}
	if opts.BaseURL == "" {
		opts.BaseURL = p.BaseURL
	}
	req := BuildRequest(model, t, opts)
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if os.Getenv("SCODE_DEBUG_REQ") == "1" {
		fmt.Fprintf(os.Stderr, "[scode] request: %s\n", body)
	}

	endpoint := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse",
		strings.TrimRight(opts.BaseURL, "/"), model.ID)

	out := make(chan llm.Event, 64)
	go func() {
		defer close(out)
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, p.transport().Timeout())
			defer cancel()
		}
		headers := map[string][]string{
			"content-type":   {"application/json"},
			"accept":         {"text/event-stream"},
			"x-goog-api-key": {key},
		}
		for k, v := range opts.ExtraHeaders {
			headers[strings.ToLower(k)] = []string{v}
		}
		resp, err := llm.PostStreamWithRetry(ctx, p.HTTP, endpoint, headers, body, p.transport())
		if err != nil {
			p.emitError(out, model.ID, err.Error())
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			p.emitError(out, model.ID, fmt.Sprintf("google: HTTP %d: %s", resp.StatusCode, readErrorBody(resp.Body)))
			return
		}
		p.pump(ctx, resp.Body, model.ID, out)
	}()
	return out, nil
}

// pump drives the SSE stream through the assembler (pi's stream loop in
// google-generative-ai.ts): chunks carry parts that extend the current
// text/thinking block or complete function calls atomically.
func (p *Provider) pump(ctx context.Context, r io.Reader, model string, out chan<- llm.Event) {
	asm := newAssembler(model)
	debug := os.Getenv("SCODE_DEBUG_SSE") == "1"
	// pi pushes start right after the request is accepted, before any
	// stream event.
	{
		msg := asm.snapshot()
		out <- llm.Event{Type: llm.EventStart, Message: &msg}
	}
	err := llm.ReadSSE(r, func(ev llm.SSEEvent) error {
		if debug {
			fmt.Fprintf(os.Stderr, "[scode] sse: %s\n", ev.Data)
		}
		events, err := asm.handleChunk(ev.Data)
		for _, e := range events {
			select {
			case out <- e:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return err
	})
	// Stream settled: on a clean EOF close any open block (pi finalizes
	// currentBlock after the loop); the exception path goes straight to
	// the terminal error like pi's catch.
	if err == nil {
		for _, e := range asm.finish() {
			select {
			case out <- e:
			case <-ctx.Done():
				return
			}
		}
	}
	if err != nil {
		asm.fail(err.Error())
	} else if ctx.Err() != nil {
		asm.abort()
	} else if asm.stopReason == "" {
		asm.fail("Google stream ended without a finish reason")
	} else if asm.stopReason == llm.StopError {
		asm.fail("Provider stopped with: " + asm.rawStop)
	}
	msg := asm.snapshot()
	if msg.StopReason == llm.StopError || msg.StopReason == llm.StopAborted {
		out <- llm.Event{Type: llm.EventError, Message: &msg, Reason: msg.StopReason, Err: fmt.Errorf("%s", msg.Error)}
		return
	}
	out <- llm.Event{Type: llm.EventDone, Message: &msg, Reason: msg.StopReason}
}

func (p *Provider) emitError(out chan<- llm.Event, model, text string) {
	msg := llm.Message{
		Role:       llm.RoleAssistant,
		Model:      model,
		Provider:   p.Name(),
		StopReason: llm.StopError,
		Error:      text,
	}
	out <- llm.Event{Type: llm.EventError, Message: &msg, Reason: llm.StopError, Err: fmt.Errorf("%s", text)}
}

// readErrorBody surfaces the Gemini error payload
// ({"error": {code, message, status}}).
func readErrorBody(r io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(r, 64<<10))
	if err != nil || len(b) == 0 {
		return "(no body)"
	}
	var e struct {
		Error struct {
			Code    int    `json:"code"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
		if e.Error.Status != "" {
			return e.Error.Status + ": " + e.Error.Message
		}
		return e.Error.Message
	}
	return string(b)
}

// toolCallCounter generates unique fallback tool call ids
// (pi's module-level counter).
var toolCallCounter atomic.Int64

func fallbackToolCallID(name string, existing func(string) bool) string {
	for {
		id := fmt.Sprintf("%s_%d_%d", name, time.Now().UnixMilli(), toolCallCounter.Add(1))
		if !existing(id) {
			return id
		}
	}
}
