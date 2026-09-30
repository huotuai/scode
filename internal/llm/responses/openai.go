package responses

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"scode/internal/llm"
)

// OpenAIProvider streams assistant turns over the OpenAI Responses API
// (pi's openai-responses.ts). Same protocol family as Azure's Responses
// endpoint; conversion and stream assembly live in convert.go/stream.go.
type OpenAIProvider struct {
	Key     string
	BaseURL string
	HTTP    *http.Client
	// Transport carries pi's settings.retry.provider knobs
	// (timeout, attempts, backoff cap).
	Transport llm.TransportConfig
}

// OpenAIDefaultBaseURL is pi's openai provider baseUrl.
const OpenAIDefaultBaseURL = "https://api.openai.com/v1"

// openAIToolCallProviders is pi's OPENAI_TOOL_CALL_PROVIDERS mapped onto
// scode's provider registry (pi's "openai"/"openai-codex"/"opencode" all
// speak the Responses protocol; scode registers them as one provider).
var openAIToolCallProviders = map[string]bool{"openai-responses": true}

func NewOpenAI(key, baseURL string) *OpenAIProvider {
	if baseURL == "" {
		baseURL = OpenAIDefaultBaseURL
	}
	return &OpenAIProvider{Key: key, BaseURL: baseURL, HTTP: http.DefaultClient}
}

func (p *OpenAIProvider) Name() string { return "openai-responses" }

func (p *OpenAIProvider) Caps() llm.Capabilities {
	return llm.Capabilities{
		// Prompt caching is implicit prefix matching routed by
		// prompt_cache_key; long retention maps to
		// prompt_cache_retention:"24h" (pi's compat default).
		LongCacheRetention:     true,
		StreamingToolArguments: true,
	}
}

func (p *OpenAIProvider) transport() llm.TransportConfig { return p.Transport }

func (p *OpenAIProvider) resolveKey(opts llm.StreamOptions) string {
	if opts.APIKey != "" {
		return opts.APIKey
	}
	if p.Key != "" {
		return p.Key
	}
	for _, env := range []string{"OPENAI_API_KEY"} {
		if v := os.Getenv(env); v != "" {
			return v
		}
	}
	return ""
}

func (p *OpenAIProvider) Stream(ctx context.Context, model llm.Model, t *llm.Transcript, opts llm.StreamOptions) (<-chan llm.Event, error) {
	key := p.resolveKey(opts)
	if key == "" {
		return nil, fmt.Errorf("openai-responses: no API key (set OPENAI_API_KEY or pass StreamOptions.APIKey)")
	}
	if opts.BaseURL == "" {
		opts.BaseURL = p.BaseURL
	}
	req := buildOpenAIParams(model, t, opts)
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
			ctx, cancel = context.WithTimeout(ctx, p.transport().Timeout())
			defer cancel()
		}
		headers := map[string][]string{
			"content-type":  {"application/json"},
			"accept":        {"text/event-stream"},
			"authorization": {"Bearer " + key},
		}
		// Session-affinity headers (pi's createClient): the format is
		// endpoint-specific; openrouter.ai takes x-session-id, OpenAI
		// proper takes session_id + x-client-request-id.
		retention := llm.ResolveCacheRetention(opts.Cache)
		if sid := opts.PromptCacheKey; sid != "" && retention != llm.CacheNone {
			if strings.Contains(opts.BaseURL, "openrouter.ai") {
				headers["x-session-id"] = []string{sid}
			} else {
				headers["session_id"] = []string{sid}
				headers["x-client-request-id"] = []string{sid}
			}
		}
		for k, v := range opts.ExtraHeaders {
			headers[strings.ToLower(k)] = []string{v}
		}
		resp, err := llm.PostStreamWithRetry(ctx, p.HTTP, strings.TrimRight(opts.BaseURL, "/")+"/responses", headers, body, p.transport())
		if err != nil {
			p.emitError(out, model.ID, err.Error())
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			p.emitError(out, model.ID, fmt.Sprintf("openai-responses: HTTP %d: %s", resp.StatusCode, readErrorBody(resp.Body)))
			return
		}
		p.pump(ctx, resp.Body, model.ID, out)
	}()
	return out, nil
}

// buildOpenAIParams is pi's openai-responses buildParams with default
// compat flags (developer role on, strict mode off, max_output_tokens on,
// 24h long retention).
func buildOpenAIParams(model llm.Model, t *llm.Transcript, opts llm.StreamOptions) *requestParams {
	msgs := t.Messages()
	retention := llm.ResolveCacheRetention(opts.Cache)

	params := &requestParams{
		Model:  model.ID,
		Input:  convertMessages(model, msgs, openAIToolCallProviders),
		Stream: true,
		Store:  false, // stateless multi-turn replay (pi)
	}
	if retention != llm.CacheNone {
		params.PromptCacheKey = clampPromptCacheKey(opts.PromptCacheKey)
		if retention == llm.CacheLong {
			params.PromptCacheRetention = "24h"
		}
	}

	maxTokens := opts.MaxTokens
	if maxTokens == 0 {
		maxTokens = model.MaxTokens
	}
	if maxTokens > 0 && maxTokens < minOutputTokens {
		maxTokens = minOutputTokens
	}
	params.MaxOutputTokens = maxTokens

	if opts.Temperature != 0 {
		params.Temperature = &opts.Temperature
	}

	if tools := llm.CurrentTools(msgs); len(tools) > 0 {
		params.Tools = convertTools(tools, false)
	}

	applyReasoning(params, model, opts.ThinkingLevel)
	return params
}

// applyReasoning maps the thinking level onto the Responses reasoning
// parameter (pi's buildParams reasoning block): an explicit level sends
// {effort, summary:auto} and asks for encrypted reasoning content so
// stateless replay keeps working; reasoning models with no level get an
// explicit effort:"none" (pi's default path when thinkingLevelMap has no
// off override). Non-reasoning models never see the parameter.
func applyReasoning(params *requestParams, model llm.Model, level string) {
	if !model.Reasoning {
		return
	}
	switch level {
	case "", "off":
		params.Reasoning = &wireReasoning{Effort: "none"}
	default:
		params.Reasoning = &wireReasoning{Effort: level, Summary: "auto"}
		params.Include = []string{"reasoning.encrypted_content"}
	}
}

// clampPromptCacheKey truncates the cache routing key to the protocol's
// 64-character limit (pi's clampOpenAIPromptCacheKey).
func clampPromptCacheKey(key string) string {
	if len(key) <= 64 {
		return key
	}
	return key[:64]
}

func (p *OpenAIProvider) pump(ctx context.Context, r io.Reader, model string, out chan<- llm.Event) {
	asm := newAssembler(model, p.Name())
	debug := os.Getenv("SCODE_DEBUG_SSE") == "1"
	// pi pushes start right after the request is accepted, before any
	// stream event.
	{
		msg := asm.snapshot()
		out <- llm.Event{Type: llm.EventStart, Message: &msg}
	}
	sawTerminal := false
	err := llm.ReadSSE(r, func(ev llm.SSEEvent) error {
		if debug {
			fmt.Fprintf(os.Stderr, "[scode] sse: %s\n", ev.Data)
		}
		events, terminal, err := asm.handle(ev.Data)
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
			sawTerminal = true
			return io.EOF
		}
		return nil
	})
	if err == nil && !sawTerminal {
		// EOF before a terminal response event: synthesize the promised
		// terminal (pi throws "stream ended before a terminal response
		// event").
		err = fmt.Errorf("stream ended before a terminal event")
	}
	if err != nil && err != io.EOF {
		msg := asm.snapshot()
		msg.StopReason = llm.StopError
		if ctx.Err() != nil {
			msg.StopReason = llm.StopAborted
			msg.Error = "aborted by caller"
		} else {
			msg.Error = err.Error()
		}
		select {
		case out <- llm.Event{Type: llm.EventError, Message: &msg, Reason: msg.StopReason, Err: err}:
		case <-ctx.Done():
		}
	}
}

func (p *OpenAIProvider) emitError(out chan<- llm.Event, model, text string) {
	msg := llm.Message{
		Role:       llm.RoleAssistant,
		Model:      model,
		Provider:   p.Name(),
		StopReason: llm.StopError,
		Error:      text,
	}
	out <- llm.Event{Type: llm.EventError, Message: &msg, Reason: llm.StopError, Err: fmt.Errorf("%s", text)}
}

// readErrorBody surfaces the provider's error payload (OpenAI error
// shape: {"error": {type, message, code}}).
func readErrorBody(r io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(r, 64<<10))
	if err != nil || len(b) == 0 {
		return "(no body)"
	}
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &e) == nil {
		if e.Error.Message != "" {
			typ := e.Error.Type
			if typ == "" {
				typ = e.Error.Code
			}
			return fmt.Sprintf("%s: %s", typ, e.Error.Message)
		}
		if e.Message != "" {
			return e.Message
		}
	}
	return string(b)
}
