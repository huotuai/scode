// Package openaic implements an OpenAI-compatible chat/completions adapter
// (OpenAI, GLM, DeepSeek, OpenRouter, and most self-hosted relays). Tools,
// streaming, and implicit prefix caching ride the standard wire format.
package openaic

import (
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
	Index    int          `json:"-"` // response-side only
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"` // "function"
	Function wireFunction `json:"function"`
}

type wireMessage struct {
	Role       string         `json:"role"` // system | user | assistant | tool
	Content    any            `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Reasoning  string         `json:"reasoning_content,omitempty"` // replay of thinking (DeepSeek/GLM convention)
}

type wireTool struct {
	Type     string       `json:"type"` // "function"
	Function wireFunction `json:"function"`
}

type wireStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireRequest struct {
	Model          string             `json:"model"`
	Stream         bool               `json:"stream"`
	StreamOptions  *wireStreamOptions `json:"stream_options,omitempty"`
	Messages       []wireMessage      `json:"messages"`
	Tools          []wireTool         `json:"tools,omitempty"`
	MaxTokens      int                `json:"max_tokens,omitempty"`
	Temperature    *float64           `json:"temperature,omitempty"`
	PromptCacheKey string             `json:"prompt_cache_key,omitempty"`
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
	msgs := t.Messages()

	var wire []wireMessage
	if sys := llm.CurrentSystemMessage(msgs); sys != nil {
		if text := renderSystem(*sys); text != "" {
			wire = append(wire, wireMessage{Role: "system", Content: text})
		}
	}

	for i := 1; i < len(msgs); i++ {
		m := msgs[i]
		switch m.Role {
		case llm.RoleSystem:
			continue // collapsed above
		case llm.RoleUser:
			var parts []wireContent
			for _, b := range m.Content {
				switch b.Kind {
				case llm.BlockText:
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
			for _, b := range m.Content {
				switch b.Kind {
				case llm.BlockText:
					text = append(text, b.Text)
				case llm.BlockThinking:
					wm.Reasoning = b.Text
				case llm.BlockToolCall:
					args := string(b.Arguments)
					if args == "" {
						args = "{}"
					}
					wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
						ID:       b.ID,
						Type:     "function",
						Function: wireFunction{Name: b.Name, Parameters: json.RawMessage(args)},
					})
				}
			}
			if len(text) > 0 {
				wm.Content = strings.Join(text, "\n")
			}
			if wm.Content == nil && len(wm.ToolCalls) == 0 && wm.Reasoning == "" {
				return nil, fmt.Errorf("assistant message %d has no wireable content", i)
			}
			wire = append(wire, wm)
		case llm.RoleTool:
			// One role "tool" wire message per tool-result block;
			// providers match results to calls by tool_call_id.
			for _, b := range m.Content {
				if b.Kind != llm.BlockToolResult {
					continue
				}
				var parts []string
				for _, nb := range b.Content {
					if nb.Kind == llm.BlockText {
						parts = append(parts, nb.Text)
					}
				}
				body := strings.Join(parts, "\n")
				if b.IsError && body != "" && !strings.HasPrefix(body, "ERROR") {
					body = "ERROR: " + body
				}
				wire = append(wire, wireMessage{Role: "tool", ToolCallID: b.ID, Content: body})
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
		req.MaxTokens = 8192
	}
	if opts.Temperature != 0 {
		req.Temperature = &opts.Temperature
	}
	if opts.PromptCacheKey != "" && llm.ResolveCacheRetention(opts.Cache) != llm.CacheNone {
		req.PromptCacheKey = ClampPromptCacheKey(opts.PromptCacheKey)
	}
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
