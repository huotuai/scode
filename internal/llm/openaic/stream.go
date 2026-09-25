package openaic

import (
	"encoding/json"
	"fmt"
	"strings"

	"scode/internal/llm"
)

// chatChunk is one streaming response object (data: {...} lines).
type chatChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			Reasoning string `json:"reasoning_content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// liveToolCall accumulates one tool call across argument fragments.
type liveToolCall struct {
	block llm.Block
	order int
}

// assembler converts chat/completions chunks into scode events. Tool calls
// are keyed by delta tool_calls[].index; arguments concatenate as string
// fragments and parse once at finish. Usage arrives in the final chunk
// (stream_options.include_usage).
type assembler struct {
	model    string
	provider string

	toolCalls map[int]*liveToolCall
	text      strings.Builder
	think     strings.Builder
	started   bool
	usage     llm.Usage
	finished  string // finish_reason
	hadText   bool
	hadThink  bool
	err       string
}

func newAssembler(model string) *assembler {
	return &assembler{model: model, provider: "openai-compat", toolCalls: map[int]*liveToolCall{}}
}

func (a *assembler) snapshot() llm.Message {
	out := llm.Message{
		Role:     llm.RoleAssistant,
		Model:    a.model,
		Provider: a.provider,
		Usage:    &a.usage,
	}
	if a.text.Len() > 0 {
		out.Content = append(out.Content, llm.Block{Kind: llm.BlockText, Text: a.text.String()})
	}
	// Ordered tool calls by first appearance.
	idxs := make([]int, 0, len(a.toolCalls))
	for i := range a.toolCalls {
		idxs = append(idxs, i)
	}
	for i := 1; i < len(idxs); i++ {
		for j := i; j > 0 && a.toolCalls[idxs[j]].order < a.toolCalls[idxs[j-1]].order; j-- {
			idxs[j], idxs[j-1] = idxs[j-1], idxs[j]
		}
	}
	for _, i := range idxs {
		tc := a.toolCalls[i]
		b := tc.block
		b.Arguments = json.RawMessage(strings.TrimSpace(toJSONString(b.Arguments)))
		out.Content = append(out.Content, b)
	}
	if a.think.Len() > 0 {
		out.Content = append(out.Content, llm.Block{Kind: llm.BlockThinking, Text: a.think.String()})
	}
	out.StopReason = mapFinish(a.finished)
	if a.err != "" {
		out.StopReason = llm.StopError
		out.Error = a.err
	}
	return out
}

// toJSONString re-emits raw fragment-accumulated arguments compactly; if
// the fragments never formed valid JSON the raw text passes through.
func toJSONString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

func (a *assembler) handle(data string) ([]llm.Event, bool, error) {
	if data == "[DONE]" {
		// The protocol signals no block boundaries; synthesize end events
		// so consumers see the same lifecycle as the Anthropic adapter.
		var events []llm.Event
		if a.hadText {
			events = append(events, llm.Event{Type: llm.EventTextEnd})
		}
		if a.hadThink {
			events = append(events, llm.Event{Type: llm.EventThinkingEnd})
		}
		events = append(events, a.toolCallEnds()...)
		msg := a.snapshot()
		events = append(events, llm.Event{Type: llm.EventDone, Message: &msg, Reason: msg.StopReason})
		return events, true, nil
	}
	var c chatChunk
	if err := json.Unmarshal([]byte(data), &c); err != nil {
		return nil, false, fmt.Errorf("chunk: %w", err)
	}
	if c.Error != nil && c.Error.Message != "" {
		a.err = fmt.Sprintf("%s: %s", c.Error.Type, c.Error.Message)
		msg := a.snapshot()
		return []llm.Event{{Type: llm.EventError, Message: &msg, Reason: llm.StopError, Err: fmt.Errorf("%s", a.err)}}, true, nil
	}

	var events []llm.Event
	if !a.started {
		a.started = true
		msg := a.snapshot()
		events = append(events, llm.Event{Type: llm.EventStart, Message: &msg})
	}
	for _, ch := range c.Choices {
		if ch.Delta.Content != "" {
			if !a.hadText {
				a.hadText = true
				events = append(events, llm.Event{Type: llm.EventTextStart})
			}
			a.text.WriteString(ch.Delta.Content)
			events = append(events, llm.Event{Type: llm.EventTextDelta, Delta: ch.Delta.Content})
		}
		if ch.Delta.Reasoning != "" {
			if !a.hadThink {
				a.hadThink = true
				events = append(events, llm.Event{Type: llm.EventThinkingStart})
			}
			a.think.WriteString(ch.Delta.Reasoning)
			events = append(events, llm.Event{Type: llm.EventThinkingDelta, Delta: ch.Delta.Reasoning})
		}
		for _, tc := range ch.Delta.ToolCalls {
			live, ok := a.toolCalls[tc.Index]
			if !ok {
				live = &liveToolCall{order: len(a.toolCalls)}
				live.block = llm.Block{Kind: llm.BlockToolCall, ID: tc.ID, Name: tc.Function.Name}
				a.toolCalls[tc.Index] = live
				events = append(events, llm.Event{Type: llm.EventToolCallStart, ContentIndex: live.order})
			}
			if tc.ID != "" && live.block.ID == "" {
				live.block.ID = tc.ID
			}
			if tc.Function.Name != "" && live.block.Name == "" {
				live.block.Name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				live.block.Arguments = append(live.block.Arguments, tc.Function.Arguments...)
				events = append(events, llm.Event{Type: llm.EventToolCallDelta, ContentIndex: live.order, Delta: tc.Function.Arguments})
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			a.finished = *ch.FinishReason
		}
	}
	if c.Usage != nil {
		a.usage.Input = c.Usage.PromptTokens - c.Usage.PromptTokensDetails.CachedTokens
		a.usage.CacheRead = c.Usage.PromptTokensDetails.CachedTokens
		a.usage.Output = c.Usage.CompletionTokens
	}
	return events, false, nil
}

// toolCallEnds emits toolcall_end events for every accumulated call; call
// it when the chunk stream reaches [DONE] before the done event if callers
// need block-finalized semantics.
func (a *assembler) toolCallEnds() []llm.Event {
	var events []llm.Event
	for _, i := range a.orderedIndexes() {
		events = append(events, llm.Event{Type: llm.EventToolCallEnd, ContentIndex: a.toolCalls[i].order})
	}
	return events
}

func (a *assembler) orderedIndexes() []int {
	idxs := make([]int, 0, len(a.toolCalls))
	for i := range a.toolCalls {
		idxs = append(idxs, i)
	}
	for i := 1; i < len(idxs); i++ {
		for j := i; j > 0 && a.toolCalls[idxs[j]].order < a.toolCalls[idxs[j-1]].order; j-- {
			idxs[j], idxs[j-1] = idxs[j-1], idxs[j]
		}
	}
	return idxs
}

func mapFinish(r string) llm.StopReason {
	switch r {
	case "stop":
		return llm.StopEndTurn
	case "tool_calls", "function_call":
		return llm.StopToolUse
	case "length":
		return llm.StopLength
	case "":
		return ""
	default:
		return llm.StopReason(r)
	}
}
