// Package openaic implements an OpenAI-compatible chat/completions adapter
// (OpenAI, GLM, DeepSeek, OpenRouter, and most self-hosted relays). Tools,
// streaming, and implicit prefix caching ride the standard wire format.
package openaic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"scode/internal/llm"
)

const (
	DefaultBaseURL       = "https://api.openai.com/v1"
	promptCacheKeyMaxLen = 64
)

type wireContent struct {
	Type     string        `json:"type"` // text | image_url
	Text     string        `json:"text,omitempty"`
	ImageURL *wireImageURL `json:"image_url,omitempty"`
}

type wireImageURL struct {
	URL string `json:"url"`
}

type wireFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type wireToolCall struct {
	Index    int                  `json:"-"` // response-side only
	ID       string               `json:"id,omitempty"`
	Type     string               `json:"type,omitempty"` // "function"
	Function wireToolCallFunction `json:"function"`
}

// wireToolCallFunction is the tool-call half of the protocol: assistant
// replay carries the arguments as a JSON-encoded STRING under
// "arguments" — not the schema-style "parameters" used by tool
// declarations. Conflating the two desyncs strict endpoints (422
// missing field `arguments`).
type wireToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireMessage struct {
	Role       string         `json:"role"` // system | user | assistant | tool
	Content    any            `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	// Thinking replay rides back under the dialect key it arrived in
	// (pi's signature-driven replay; the block's Signature records the
	// key at stream time). Pointers so a forced EMPTY reasoning_content
	// (pi's DeepSeek compat) survives marshaling.
	ReasoningContent *string `json:"reasoning_content,omitempty"`
	Reasoning        *string `json:"reasoning,omitempty"`
	ReasoningText    *string `json:"reasoning_text,omitempty"`
}

type wireTool struct {
	Type     string       `json:"type"` // "function"
	Function wireFunction `json:"function"`
}

type wireStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// wireZhipuThinking is Zhipu/GLM's reasoning switch (on/off only; the
// provider sizes the budget itself).
type wireZhipuThinking struct {
	Type string `json:"type"` // "enabled" | "disabled"
}

type wireRequest struct {
	Model               string             `json:"model"`
	Stream              bool               `json:"stream"`
	StreamOptions       *wireStreamOptions `json:"stream_options,omitempty"`
	Messages            []wireMessage      `json:"messages"`
	Tools               []wireTool         `json:"tools,omitempty"`
	MaxTokens           int                `json:"max_tokens,omitempty"`
	MaxCompletionTokens int                `json:"max_completion_tokens,omitempty"`
	Temperature         *float64           `json:"temperature,omitempty"`
	PromptCacheKey      string             `json:"prompt_cache_key,omitempty"`
	ReasoningEffort     string             `json:"reasoning_effort,omitempty"` // OpenAI-style low|medium|high
	Thinking            *wireZhipuThinking `json:"thinking,omitempty"`         // Zhipu-style enabled/disabled
}

// compactJSONString normalizes raw tool-call arguments into a compact
// JSON string for the wire ("arguments" is a string field); anything
// unparseable falls back to an empty object.
func compactJSONString(raw json.RawMessage) string {
	s := string(bytes.TrimSpace(raw))
	if s == "" {
		return "{}"
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// applyThinkingWire maps the neutral ThinkingLevel onto the endpoint's
// reasoning knob: Zhipu-style endpoints (bigmodel.cn / zhipuai / z.ai)
// take a thinking on/off switch (the provider sizes the budget); others
// take OpenAI-style reasoning_effort (low|medium|high). "off" on an
// effort-style endpoint omits the field — the provider default applies.
func applyThinkingWire(req *wireRequest, baseURL, level string) {
	if level == "" {
		return
	}
	lower := strings.ToLower(baseURL)
	if strings.Contains(lower, "bigmodel.cn") || strings.Contains(lower, "zhipuai") || strings.Contains(lower, "z.ai") {
		state := "enabled"
		if level == "off" {
			state = "disabled"
		}
		req.Thinking = &wireZhipuThinking{Type: state}
		return
	}
	switch level {
	case "low", "medium", "high":
		req.ReasoningEffort = level
	}
}

// reasoningReplayFields are the dialect keys thinking may replay under
// (pi's OPENAI_COMPLETIONS_REASONING_FIELDS).
var reasoningReplayFields = []string{"reasoning", "reasoning_content", "reasoning_text"}

// isReasoningReplayField reports whether a thinking block's Signature
// names a replayable reasoning field (pi's
// isOpenAICompletionsReasoningField). Anything else — an Anthropic
// signature, a Responses stored item, an empty signature from an
// aborted/legacy stream — is not a field name and never replays here.
func isReasoningReplayField(signature string) bool {
	for _, f := range reasoningReplayFields {
		if signature == f {
			return true
		}
	}
	return false
}

// setReasoning replays joined thinking under the given dialect key.
func (m *wireMessage) setReasoning(field, text string) {
	switch field {
	case "reasoning_content":
		m.ReasoningContent = &text
	case "reasoning":
		m.Reasoning = &text
	case "reasoning_text":
		m.ReasoningText = &text
	}
}

// hasReasoning reports whether any replay field is set (a forced empty
// DeepSeek field counts — the skip-empty check has its own rule).
func (m *wireMessage) hasReasoning() bool {
	return m.ReasoningContent != nil || m.Reasoning != nil || m.ReasoningText != nil
}

// isDeepSeekURL mirrors pi's isDeepSeek detection (their current
// thinking-mode API requires reasoning_content on replayed assistant
// messages; see BuildRequest).
func isDeepSeekURL(baseURL string) bool {
	return strings.Contains(strings.ToLower(baseURL), "deepseek.com")
}

// ClampPromptCacheKey truncates the cache routing key to the protocol's
// 64-character limit (pi's clampOpenAIPromptCacheKey).
func ClampPromptCacheKey(key string) string {
	if len(key) <= promptCacheKeyMaxLen {
		return key
	}
	return key[:promptCacheKeyMaxLen]
}

// BuildRequest converts a transcript into a chat/completions request.
// System messages collapse into one leading system role message (replayed
// via CurrentSystemMessage); tool results become role "tool" messages;
// assistant tool calls become tool_calls entries. Implicit prefix caching
// keys off byte-identical prefixes, which the transcript model guarantees;
// PromptCacheKey additionally routes requests of one session to the same
// cache shard.
func BuildRequest(model llm.Model, t *llm.Transcript, opts llm.StreamOptions) (*wireRequest, error) {
	// The request-time transform drops errored/aborted turns and
	// answers orphaned tool calls in place (pi's transform-messages).
	msgs := llm.TransformMessages(t.Messages())

	var wire []wireMessage
	midConvo := model.Caps.MidConvoSystem
	if midConvo {
		// pi's verified-model path (compat.supportsMidConvoSystemMessages):
		// the head is the LEADING system message alone; later system
		// deltas ride in place below, so a mid-session section change
		// never rewrites the request head and the implicit prefix cache
		// survives it.
		if text := renderSystem(msgs[0]); text != "" {
			wire = append(wire, wireMessage{Role: "system", Content: text})
		}
	} else if sys := llm.CurrentSystemMessage(msgs); sys != nil {
		if text := renderSystem(*sys); text != "" {
			wire = append(wire, wireMessage{Role: "system", Content: text})
		}
	}

	for i := 1; i < len(msgs); i++ {
		m := msgs[i]
		switch m.Role {
		case llm.RoleSystem:
			if midConvo {
				// In-place delta: content + section patches, chronological.
				// Tool deltas never render here — the top-level tools
				// array carries them (CurrentTools replay, unchanged).
				if text := renderSystem(m); text != "" {
					wire = append(wire, wireMessage{Role: "system", Content: text})
				}
			}
			continue // default: collapsed into the head above
		case llm.RoleUser:
			var parts []wireContent
			for _, b := range m.Content {
				switch b.Kind {
				case llm.BlockText:
					if b.Text == "" {
						continue
					}
					parts = append(parts, wireContent{Type: "text", Text: b.Text})
				case llm.BlockImage:
					parts = append(parts, wireContent{Type: "image_url", ImageURL: &wireImageURL{URL: "data:" + b.MimeType + ";base64," + b.Data}})
				}
			}
			if len(parts) == 1 && parts[0].Type == "text" {
				wire = append(wire, wireMessage{Role: "user", Content: parts[0].Text})
			} else if len(parts) > 0 {
				wire = append(wire, wireMessage{Role: "user", Content: parts})
			}
		case llm.RoleAssistant:
			wm := wireMessage{Role: "assistant"}
			var text []string
			var thinking []string
			thinkField := ""
			for _, b := range m.Content {
				switch b.Kind {
				case llm.BlockText:
					text = append(text, b.Text)
				case llm.BlockThinking:
					if strings.TrimSpace(b.Text) == "" {
						continue
					}
					if thinkField == "" {
						thinkField = b.Signature // first non-empty block (pi)
					}
					thinking = append(thinking, b.Text)
				case llm.BlockToolCall:
					args := compactJSONString(b.Arguments)
					wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
						ID:       b.ID,
						Type:     "function",
						Function: wireToolCallFunction{Name: b.Name, Arguments: args},
					})
				}
			}
			if len(text) > 0 {
				wm.Content = strings.Join(text, "\n")
			}
			// pi's full replay: ALL historical thinking rides back under
			// the dialect key it arrived in (recorded as the block's
			// Signature at stream time); no valid field name = no replay.
			if len(thinking) > 0 && isReasoningReplayField(thinkField) {
				wm.setReasoning(thinkField, strings.Join(thinking, "\n"))
			}
			// pi's DeepSeek compat: with a reasoning-capable model every
			// replayed assistant message must carry reasoning_content —
			// empty when the turn produced no thinking (their API
			// rejects the missing field). Never saves an otherwise
			// empty message from the skip below (pi skips it too).
			deepSeekEmpty := false
			if model.Reasoning && isDeepSeekURL(opts.BaseURL) && wm.ReasoningContent == nil {
				empty := ""
				wm.ReasoningContent = &empty
				deepSeekEmpty = true
			}
			if wm.Content == nil && len(wm.ToolCalls) == 0 && (!wm.hasReasoning() || deepSeekEmpty) {
				continue // nothing wireable (the transform already dropped error turns)
			}
			wire = append(wire, wm)
		case llm.RoleTool:
			// One role "tool" wire message per tool-result block;
			// providers match results to calls by tool_call_id.
			// chat/completions has no image blocks in tool messages, so
			// images follow in a user message when the model accepts image
			// input (pi's shape, gated on model.input containing "image").
			for _, b := range m.Content {
				if b.Kind != llm.BlockToolResult {
					continue
				}
				var parts []string
				var images []llm.Block
				for _, nb := range b.Content {
					switch nb.Kind {
					case llm.BlockText:
						parts = append(parts, nb.Text)
					case llm.BlockImage:
						images = append(images, nb)
					}
				}
				body := strings.Join(parts, "\n")
				if strings.TrimSpace(body) == "" {
					if len(images) > 0 {
						body = "(see attached image)"
					} else {
						body = "(no output)"
					}
				}
				if b.IsError && body != "" && !strings.HasPrefix(body, "ERROR") {
					body = "ERROR: " + body
				}
				wire = append(wire, wireMessage{Role: "tool", ToolCallID: b.ID, Content: body})
				if len(images) > 0 && model.Caps.ImageInput {
					// pi gates the forwarding on model.input containing
					// "image"; text-only models keep the placeholder.
					content := []wireContent{{Type: "text", Text: "Attached image(s) from tool result:"}}
					for _, im := range images {
						content = append(content, wireContent{Type: "image_url", ImageURL: &wireImageURL{URL: "data:" + im.MimeType + ";base64," + im.Data}})
					}
					wire = append(wire, wireMessage{Role: "user", Content: content})
				}
			}
		default:
			return nil, fmt.Errorf("message %d: unknown role %q", i, m.Role)
		}
	}
	if len(wire) == 0 {
		return nil, fmt.Errorf("transcript has no conversation messages")
	}

	var tools []wireTool
	for _, tl := range llm.CurrentTools(msgs) {
		var params any
		if len(tl.Parameters) > 0 {
			params = tl.Parameters
		}
		tools = append(tools, wireTool{Type: "function", Function: wireFunction{Name: tl.Name, Description: tl.Description, Parameters: params}})
	}

	req := &wireRequest{
		Model:         model.ID,
		Stream:        true,
		StreamOptions: &wireStreamOptions{IncludeUsage: true},
		Messages:      wire,
		Tools:         tools,
		MaxTokens:     opts.MaxTokens,
	}
	if req.MaxTokens == 0 {
		// pi's buildBaseOptions: fall back to the model's catalog cap,
		// then a flat default.
		req.MaxTokens = model.MaxTokens
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = 8192
	}
	// OpenAI proper rejects max_tokens on reasoning models; relays
	// (GLM/DeepSeek/…) universally expect it.
	if strings.Contains(strings.ToLower(opts.BaseURL), "api.openai.com") {
		req.MaxCompletionTokens = req.MaxTokens
		req.MaxTokens = 0
	}
	if opts.Temperature != 0 {
		req.Temperature = &opts.Temperature
	}
	if opts.PromptCacheKey != "" && llm.ResolveCacheRetention(opts.Cache) != llm.CacheNone {
		req.PromptCacheKey = ClampPromptCacheKey(opts.PromptCacheKey)
	}
	applyThinkingWire(req, opts.BaseURL, opts.ThinkingLevel)
	return req, nil
}

// renderSystem matches the anthropic adapter's section rendering so the
// same transcript produces equivalent prompts across protocols.
func renderSystem(m llm.Message) string {
	base := ""
	for _, b := range m.Content {
		if b.Kind == llm.BlockText {
			if base != "" {
				base += "\n"
			}
			base += b.Text
		}
	}
	if len(m.Sections) == 0 {
		return base
	}
	out := base
	for _, s := range m.Sections {
		if out != "" {
			out += "\n\n"
		}
		out += "<" + s.Name + ">\n" + s.Value + "\n</" + s.Name + ">"
	}
	return out
}
