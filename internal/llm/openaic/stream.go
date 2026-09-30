package openaic

import (
	"encoding/json"
	"fmt"
	"strings"

	"scode/internal/llm"
)

// chatChunk is one streaming response object (data: {...} lines).
// Reasoning arrives under different keys per dialect (reasoning_content
// for DeepSeek/GLM, reasoning for many relays, reasoning_text for
// llama.cpp — pi's first-non-empty rule); usage cache fields live in
// different spots too (prompt_tokens_details.cached_tokens standard,
// prompt_cache_hit_tokens DeepSeek, top-level cached_tokens Kimi,
// prompt_tokens_details.cache_write_tokens OpenRouter).
type chatChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role         string `json:"role"`
			Content      string `json:"content"`
			Reasoning    string `json:"reasoning_content"`
			ReasoningAlt string `json:"reasoning"`
			ReasoningLlm string `json:"reasoning_text"`
			ToolCalls    []struct {
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
			WriteTokens  int64 `json:"cache_write_tokens"`
		} `json:"prompt_tokens_details"`
		CacheHitTokens int64 `json:"prompt_cache_hit_tokens"` // DeepSeek
		CachedTokens   int64 `json:"cached_tokens"`           // Kimi (top-level)
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
	// thinkField records WHICH dialect key the reasoning stream arrived
	// under (reasoning_content / reasoning / reasoning_text) — pi's
	// thinkingSignature. It rides the finished block's Signature so the
	// request builder can replay the thinking under the same field.
	thinkField string
	started    bool
	usage      llm.Usage
	finished   string // finish_reason
	hadText    bool
	hadThink   bool
	err        string
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
		out.Content = append(out.Content, llm.Block{Kind: llm.BlockThinking, Text: a.think.String(), Signature: a.thinkField})
	}
	out.StopReason = mapFinish(a.finished)
	if a.err != "" {
		out.StopReason = llm.StopError
		out.Error = a.err
	}
	return out
}

// toJSONString re-emits raw fragment-accumulated arguments compactly.
// Fragments that never formed valid JSON (truncated streams, relay
// quirks) are wrapped as a JSON string so the block is always
// marshalable; argument validation then rejects it cleanly.
func toJSONString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var v any
	if err := json.Unmarshal(raw, &v); err == nil {
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
	}
	if b, err := json.Marshal(string(raw)); err == nil {
		return string(b)
	}
	return "{}"
}

func (a *assembler) handle(data string) ([]llm.Event, bool, error) {
	if data == "[DONE]" {
		return a.conclude(), true, nil
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
		if field, reasoning := reasoningDelta(ch.Delta.Reasoning, ch.Delta.ReasoningAlt, ch.Delta.ReasoningLlm); reasoning != "" {
			if a.thinkField == "" {
				a.thinkField = field // first-seen field wins (pi's ensureThinkingBlock)
			}
			if !a.hadThink {
				a.hadThink = true
				events = append(events, llm.Event{Type: llm.EventThinkingStart})
			}
			a.think.WriteString(reasoning)
			events = append(events, llm.Event{Type: llm.EventThinkingDelta, Delta: reasoning})
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
			switch *ch.FinishReason {
			case "network_error", "error", "content_filter":
				a.err = fmt.Sprintf("provider reported finish_reason=%s", *ch.FinishReason)
			default:
				a.finished = *ch.FinishReason
			}
		}
	}
	if c.Usage != nil {
		cacheRead := firstNonZero(
			c.Usage.PromptTokensDetails.CachedTokens, // standard / OpenRouter
			c.Usage.CacheHitTokens,                   // DeepSeek
			c.Usage.CachedTokens,                     // Kimi (top-level)
		)
		cacheWrite := c.Usage.PromptTokensDetails.WriteTokens // OpenRouter
		a.usage.CacheRead = cacheRead
		a.usage.CacheWrite = cacheWrite
		a.usage.Input = c.Usage.PromptTokens - cacheRead - cacheWrite
		if a.usage.Input < 0 {
			a.usage.Input = 0
		}
		a.usage.Output = c.Usage.CompletionTokens
	}
	return events, false, nil
}

// conclude synthesizes the terminal event sequence (block boundaries +
// done/error) the Anthropic adapter defines. The protocol signals no block
// boundaries, so they are fabricated here. Called on [DONE] and, for streams
// that close without the sentinel, whenever a finish_reason already proved
// generation completed (see Provider.pump).
func (a *assembler) conclude() []llm.Event {
	var events []llm.Event
	if a.hadText {
		events = append(events, llm.Event{Type: llm.EventTextEnd})
	}
	if a.hadThink {
		events = append(events, llm.Event{Type: llm.EventThinkingEnd})
	}
	events = append(events, a.toolCallEnds()...)
	msg := a.snapshot()
	if a.err != "" {
		// Some compat providers (GLM) report mid-generation failures
		// as an error-ish finish_reason with empty content.
		return append(events, llm.Event{Type: llm.EventError, Message: &msg, Reason: llm.StopError, Err: fmt.Errorf("%s", a.err)})
	}
	return append(events, llm.Event{Type: llm.EventDone, Message: &msg, Reason: msg.StopReason})
}

// reasoningDelta picks the reasoning payload out of a stream delta and
// reports which dialect key carried it (pi's reasoningFields priority:
// reasoning_content, then reasoning, then reasoning_text).
func reasoningDelta(content, alt, llm string) (field, text string) {
	switch {
	case content != "":
		return "reasoning_content", content
	case alt != "":
		return "reasoning", alt
	case llm != "":
		return "reasoning_text", llm
	}
	return "", ""
}

func firstNonZero(ns ...int64) int64 {
	for _, n := range ns {
		if n != 0 {
			return n
		}
	}
	return 0
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
