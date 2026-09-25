// Package anthropic implements the Anthropic Messages API adapter:
// transcript → wire request with pi's cache_control breakpoint placement,
// and SSE response → llm.Event stream assembly.
package anthropic

import (
	"encoding/json"
	"fmt"

	"scode/internal/llm"
)

const (
	DefaultBaseURL = "https://api.anthropic.com"
	APIVersion     = "2023-06-01"
	DefaultMaxOut  = 8192
)

// cacheControl is the Anthropic ephemeral cache marker.
type cacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"` // "1h" when long retention is supported
}

// wireBlock is one Anthropic content block (flat, kind-discriminated).
type wireBlock struct {
	Type string `json:"type"` // text | thinking | tool_use | tool_result | image

	Text string `json:"text,omitempty"` // text

	Thinking  string `json:"thinking,omitempty"`  // thinking
	Signature string `json:"signature,omitempty"` // thinking
	Data      string `json:"data,omitempty"`      // redacted_thinking (opaque)

	ID    string          `json:"id,omitempty"`    // tool_use
	Name  string          `json:"name,omitempty"`  // tool_use
	Input json.RawMessage `json:"input,omitempty"` // tool_use

	ToolUseID string        `json:"tool_use_id,omitempty"` // tool_result
	Content   []wireBlock   `json:"content,omitempty"`     // tool_result
	IsError   bool          `json:"is_error,omitempty"`    // tool_result
	Source    *wireSource   `json:"source,omitempty"`      // image
	CacheCtl  *cacheControl `json:"cache_control,omitempty"`
}

type wireSource struct {
	Type      string `json:"type"` // base64
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type wireMessage struct {
	Role    string      `json:"role"` // user | assistant
	Content []wireBlock `json:"content"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	CacheCtl    *cacheControl   `json:"cache_control,omitempty"`
}

type wireThinking struct {
	Type         string `json:"type"` // "enabled" — required by the API
	BudgetTokens int    `json:"budget_tokens"`
}

type wireRequest struct {
	Model       string        `json:"model"`
	MaxTokens   int           `json:"max_tokens"`
	Stream      bool          `json:"stream"`
	System      []wireBlock   `json:"system,omitempty"`
	Tools       []wireTool    `json:"tools,omitempty"`
	Messages    []wireMessage `json:"messages"`
	Temperature *float64      `json:"temperature,omitempty"`
	Thinking    *wireThinking `json:"thinking,omitempty"`
}

// thinkingBudgets maps scode thinking levels to token budgets.
var thinkingBudgets = map[string]int{"low": 2048, "medium": 8192, "high": 16384}

// BuildRequest converts a transcript into a Messages API request body.
//
// Cache strategy (pi's placement, ≤4 breakpoints): the last tool
// declaration, the system block, and the tail block of the last user
// message carry cache_control. System messages are collapsed via
// CurrentSystemMessage (the protocol has no mid-conversation system), and
// the current tool set rides the top-level tools array — the transcript
// prefix itself never changes, so message-prefix caching holds even when
// the tool set evolves.
func BuildRequest(model llm.Model, t *llm.Transcript, opts llm.StreamOptions) (*wireRequest, error) {
	msgs := t.Messages()
	retention := llm.ResolveCacheRetention(opts.Cache)
	markCache := retention != llm.CacheNone

	var cc *cacheControl
	if markCache {
		cc = &cacheControl{Type: "ephemeral"}
		if retention == llm.CacheLong && model.Caps.LongCacheRetention {
			cc.TTL = "1h"
		}
	}

	// System: replay every system message into one top-level block.
	var system []wireBlock
	if sys := llm.CurrentSystemMessage(msgs); sys != nil {
		if text := renderSystem(*sys); text != "" {
			system = []wireBlock{{Type: "text", Text: text, CacheCtl: cc}}
		}
	}

	// Tools: the current set, breakpoint on the last declaration.
	tools := llm.CurrentTools(msgs)
	var wireTools []wireTool
	for i, tl := range tools {
		schema := tl.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		wireTools = append(wireTools, wireTool{
			Name:        tl.Name,
			Description: tl.Description,
			InputSchema: schema,
			CacheCtl: func() *cacheControl {
				if markCache && i == len(tools)-1 {
					return cc
				}
				return nil
			}(),
		})
	}

	// Messages: fold tool results into user messages, merge consecutive
	// same-role messages (the API requires user/assistant alternation).
	// tool_use ids without a matching tool_result are synthesized as error
	// results — otherwise one aborted turn mid-tool bricks every later
	// request with a 400 (pi's transform-messages does the same).
	var wire []wireMessage
	pending := map[string]bool{} // tool_use ids awaiting a result
	var pendingOrder []string
	for i := 1; i < len(msgs); i++ {
		m := msgs[i]
		switch m.Role {
		case llm.RoleSystem:
			continue // collapsed into top-level system above
		case llm.RoleUser, llm.RoleTool:
			role := "user"
			var blocks []wireBlock
			for _, b := range m.Content {
				if b.Kind == llm.BlockText && b.Text == "" {
					continue // the API rejects empty text blocks
				}
				wb := convertOutgoingBlock(b)
				if b.Kind == llm.BlockToolResult && b.ID != "" {
					delete(pending, b.ID)
				}
				blocks = append(blocks, wb)
			}
			if n := len(wire); n > 0 && wire[n-1].Role == role {
				wire[n-1].Content = append(wire[n-1].Content, blocks...)
			} else {
				wire = append(wire, wireMessage{Role: role, Content: blocks})
			}
		case llm.RoleAssistant:
			var blocks []wireBlock
			for _, b := range m.Content {
				if b.Kind == llm.BlockText && b.Text == "" {
					continue // the API rejects empty text blocks
				}
				wb := convertOutgoingBlock(b)
				if b.Kind == llm.BlockToolCall && b.ID != "" {
					if !pending[b.ID] {
						pending[b.ID] = true
						pendingOrder = append(pendingOrder, b.ID)
					}
				}
				blocks = append(blocks, wb)
			}
			if len(blocks) == 0 {
				if m.Error == "" {
					return nil, fmt.Errorf("assistant message %d has no wireable content", i)
				}
				// Recorded failure turns (including legacy content-less
				// ones) replay as plain text instead of poisoning the
				// session.
				blocks = []wireBlock{{Type: "text", Text: "[turn failed: " + m.Error + "]"}}
			}
			if n := len(wire); n > 0 && wire[n-1].Role == "assistant" {
				wire[n-1].Content = append(wire[n-1].Content, blocks...)
			} else {
				wire = append(wire, wireMessage{Role: "assistant", Content: blocks})
			}
		default:
			return nil, fmt.Errorf("message %d: unknown role %q", i, m.Role)
		}
	}
	if len(wire) == 0 {
		return nil, fmt.Errorf("transcript has no conversation messages")
	}
	if len(pendingOrder) > 0 {
		var synth []wireBlock
		for _, id := range pendingOrder {
			if !pending[id] {
				continue
			}
			synth = append(synth, wireBlock{
				Type:      "tool_result",
				ToolUseID: id,
				Content:   []wireBlock{{Type: "text", Text: "No result provided (the turn was interrupted)."}},
				IsError:   true,
			})
		}
		if len(synth) > 0 {
			wire = append(wire, wireMessage{Role: "user", Content: synth})
		}
	}

	// Breakpoint on the tail block of the last user message: conversation
	// history caching.
	if markCache {
		if n := len(wire); n > 0 && wire[n-1].Role == "user" && len(wire[n-1].Content) > 0 {
			last := &wire[n-1].Content[len(wire[n-1].Content)-1]
			if last.Type == "text" || last.Type == "image" || last.Type == "tool_result" {
				last.CacheCtl = cc
			}
		}
	}

	maxTokens := opts.MaxTokens
	if maxTokens == 0 {
		maxTokens = DefaultMaxOut
	}

	req := &wireRequest{
		Model:     model.ID,
		MaxTokens: maxTokens,
		Stream:    true,
		System:    system,
		Tools:     wireTools,
		Messages:  wire,
	}

	level := opts.ThinkingLevel
	if level != "" && level != "off" {
		budget, ok := thinkingBudgets[level]
		if !ok {
			return nil, fmt.Errorf("unknown thinking level %q", level)
		}
		if budget >= maxTokens {
			return nil, fmt.Errorf("thinking budget %d must be below max_tokens %d", budget, maxTokens)
		}
		req.Thinking = &wireThinking{Type: "enabled", BudgetTokens: budget}
		// The API rejects temperature alongside extended thinking.
		req.Temperature = nil
	} else if opts.Temperature != 0 {
		req.Temperature = &opts.Temperature
	}
	return req, nil
}

// convertOutgoingBlock maps a provider-neutral block to the wire shape.
func convertOutgoingBlock(b llm.Block) wireBlock {
	switch b.Kind {
	case llm.BlockText:
		return wireBlock{Type: "text", Text: b.Text}
	case llm.BlockThinking:
		if b.Redacted {
			return wireBlock{Type: "redacted_thinking", Data: b.Text}
		}
		return wireBlock{Type: "thinking", Thinking: b.Text, Signature: b.Signature}
	case llm.BlockToolCall:
		input := b.Arguments
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		}
		return wireBlock{Type: "tool_use", ID: b.ID, Name: b.Name, Input: input}
	case llm.BlockToolResult:
		var content []wireBlock
		for _, nb := range b.Content {
			if nb.Kind == llm.BlockText && nb.Text == "" {
				continue
			}
			content = append(content, convertOutgoingBlock(nb))
		}
		if len(content) == 0 && b.Text != "" {
			content = []wireBlock{{Type: "text", Text: b.Text}}
		}
		if len(content) == 0 {
			content = []wireBlock{{Type: "text", Text: "(no output)"}}
		}
		return wireBlock{Type: "tool_result", ToolUseID: b.ID, Content: content, IsError: b.IsError}
	case llm.BlockImage:
		return wireBlock{Type: "image", Source: &wireSource{Type: "base64", MediaType: b.MimeType, Data: b.Data}}
	default:
		return wireBlock{Type: "text", Text: b.Text}
	}
}

// renderSystem produces the deterministic system text: base content first,
// then each replayed section wrapped in <name> tags (pi's section shape).
func renderSystem(m llm.Message) string {
	base := systemText(m)
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

func systemText(m llm.Message) string {
	s := ""
	for i, b := range m.Content {
		if b.Kind != llm.BlockText {
			continue
		}
		if i > 0 && s != "" {
			s += "\n"
		}
		s += b.Text
	}
	return s
}
