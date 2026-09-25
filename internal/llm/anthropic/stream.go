package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"

	"scode/internal/llm"
)

// SSE payload shapes (subset actually consumed).

type sseError struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type messageStart struct {
	Message struct {
		ID    string `json:"id"`
		Usage struct {
			InputTokens              int64 `json:"input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

type contentBlockStart struct {
	Index        int `json:"index"`
	ContentBlock struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		Data      string          `json:"data"` // redacted_thinking (opaque)
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		Signature string          `json:"signature"`
	} `json:"content_block"`
}

type contentBlockDelta struct {
	Index int `json:"index"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
}

type messageDelta struct {
	Delta struct {
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage struct {
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

// liveBlock is a content block under assembly, keyed by wire index.
type liveBlock struct {
	block       llm.Block
	partialJSON string
	index       int // position in the final message content
}

// assembler turns the SSE event sequence into llm events and one final
// assistant message, mirroring pi's anthropic-messages streaming logic:
// blocks accumulate keyed by index; tool arguments accumulate as raw JSON
// fragments and parse once at content_block_stop; usage arrives split
// across message_start and message_delta.
type assembler struct {
	model    string
	provider string

	blocks map[int]*liveBlock
	usage  llm.Usage
	msg    llm.Message
}

func newAssembler(model string) *assembler {
	return &assembler{model: model, provider: "anthropic", blocks: map[int]*liveBlock{}}
}

func (a *assembler) snapshot() llm.Message {
	out := llm.Message{
		Role:     llm.RoleAssistant,
		TS:       0, // stamped by the caller when persisted
		Model:    a.model,
		Provider: a.provider,
		Usage:    &a.usage,
	}
	for _, lb := range a.ordered() {
		b := lb.block
		if b.Kind == llm.BlockToolCall {
			args := compactArgs(lb.partialJSON)
			b.Arguments = args
		}
		out.Content = append(out.Content, b)
	}
	out.StopReason = a.msg.StopReason
	out.Error = a.msg.Error
	return out
}

// compactArgs normalizes accumulated tool-call argument fragments. An
// incomplete stream can leave invalid JSON (abort mid-args, proxy
// truncation); falling back to {} keeps the persisted message replayable
// instead of poisoning the transcript with corrupt tool_use input.
func compactArgs(partial string) json.RawMessage {
	s := strings.TrimSpace(partial)
	if s == "" {
		return json.RawMessage(`{}`)
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return json.RawMessage(`{}`)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func (a *assembler) ordered() []*liveBlock {
	var idxs []int
	for i := range a.blocks {
		idxs = append(idxs, i)
	}
	// Wire indexes arrive in order; simple insertion keeps content order.
	for i := 1; i < len(idxs); i++ {
		for j := i; j > 0 && idxs[j] < idxs[j-1]; j-- {
			idxs[j], idxs[j-1] = idxs[j-1], idxs[j]
		}
	}
	out := make([]*liveBlock, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, a.blocks[i])
	}
	return out
}

// handle processes one SSE event. It returns the scode-level events to emit
// and whether the stream is now terminal.
func (a *assembler) handle(name, data string) ([]llm.Event, bool, error) {
	switch name {
	case "message_start":
		var ms messageStart
		if err := json.Unmarshal([]byte(data), &ms); err != nil {
			return nil, false, fmt.Errorf("message_start: %w", err)
		}
		a.usage.Input = ms.Message.Usage.InputTokens
		a.usage.CacheWrite = ms.Message.Usage.CacheCreationInputTokens
		a.usage.CacheRead = ms.Message.Usage.CacheReadInputTokens
		msg := a.snapshot()
		return []llm.Event{{Type: llm.EventStart, Message: &msg}}, false, nil

	case "content_block_start":
		var cbs contentBlockStart
		if err := json.Unmarshal([]byte(data), &cbs); err != nil {
			return nil, false, fmt.Errorf("content_block_start: %w", err)
		}
		lb := &liveBlock{index: len(a.blocks)}
		var ev llm.EventType
		switch cbs.ContentBlock.Type {
		case "text":
			lb.block = llm.Block{Kind: llm.BlockText, Text: cbs.ContentBlock.Text}
			ev = llm.EventTextStart
		case "thinking":
			lb.block = llm.Block{Kind: llm.BlockThinking, Text: cbs.ContentBlock.Thinking, Signature: cbs.ContentBlock.Signature}
			ev = llm.EventThinkingStart
		case "redacted_thinking":
			// Opaque reasoning the provider encrypted; complete at start,
			// no deltas, must replay verbatim.
			lb.block = llm.Block{Kind: llm.BlockThinking, Text: cbs.ContentBlock.Data, Redacted: true}
			ev = llm.EventThinkingStart
		case "tool_use":
			lb.block = llm.Block{Kind: llm.BlockToolCall, ID: cbs.ContentBlock.ID, Name: cbs.ContentBlock.Name}
			ev = llm.EventToolCallStart
		default:
			// Unknown block types are skipped entirely (events AND
			// snapshot) — a zero-kind block in the persisted message
			// would replay as an empty text block the API rejects.
			return nil, false, nil
		}
		a.blocks[cbs.Index] = lb
		return []llm.Event{{Type: ev, ContentIndex: lb.index}}, false, nil

	case "content_block_delta":
		var cbd contentBlockDelta
		if err := json.Unmarshal([]byte(data), &cbd); err != nil {
			return nil, false, fmt.Errorf("content_block_delta: %w", err)
		}
		lb, ok := a.blocks[cbd.Index]
		if !ok {
			return nil, false, nil
		}
		switch cbd.Delta.Type {
		case "text_delta":
			lb.block.Text += cbd.Delta.Text
			return []llm.Event{{Type: llm.EventTextDelta, ContentIndex: lb.index, Delta: cbd.Delta.Text}}, false, nil
		case "thinking_delta":
			lb.block.Text += cbd.Delta.Thinking
			return []llm.Event{{Type: llm.EventThinkingDelta, ContentIndex: lb.index, Delta: cbd.Delta.Thinking}}, false, nil
		case "signature_delta":
			lb.block.Signature += cbd.Delta.Signature
			return nil, false, nil
		case "input_json_delta":
			lb.partialJSON += cbd.Delta.PartialJSON
			return []llm.Event{{Type: llm.EventToolCallDelta, ContentIndex: lb.index, Delta: cbd.Delta.PartialJSON}}, false, nil
		default:
			return nil, false, nil
		}

	case "content_block_stop":
		lb, ok := a.blocks[parseIndex(data)]
		if !ok || lb.block.Kind == "" {
			return nil, false, nil
		}
		var ev llm.EventType
		switch lb.block.Kind {
		case llm.BlockText:
			ev = llm.EventTextEnd
		case llm.BlockThinking:
			ev = llm.EventThinkingEnd
		case llm.BlockToolCall:
			ev = llm.EventToolCallEnd
		default:
			return nil, false, nil
		}
		return []llm.Event{{Type: ev, ContentIndex: lb.index}}, false, nil

	case "message_delta":
		var md messageDelta
		if err := json.Unmarshal([]byte(data), &md); err != nil {
			return nil, false, fmt.Errorf("message_delta: %w", err)
		}
		a.usage.Output = md.Usage.OutputTokens
		a.msg.StopReason = mapStopReason(md.Delta.StopReason)
		if a.msg.StopReason == llm.StopError {
			a.msg.Error = "provider stopped with reason " + md.Delta.StopReason
		}
		return nil, false, nil

	case "message_stop":
		msg := a.snapshot()
		return []llm.Event{{Type: llm.EventDone, Message: &msg, Reason: a.msg.StopReason}}, true, nil

	case "error":
		var se sseError
		if err := json.Unmarshal([]byte(data), &se); err != nil {
			se.Error.Message = data
		}
		a.msg.StopReason = llm.StopError
		a.msg.Error = fmt.Sprintf("%s: %s", se.Error.Type, se.Error.Message)
		msg := a.snapshot()
		return []llm.Event{{Type: llm.EventError, Message: &msg, Reason: llm.StopError, Err: fmt.Errorf("%s", a.msg.Error)}}, true, nil

	default: // ping and unknown events
		return nil, false, nil
	}
}

func mapStopReason(r string) llm.StopReason {
	switch r {
	case "end_turn", "stop_sequence", "pause_turn":
		// pause_turn/stop_sequence are normal completions for this loop
		// (pi resubmits pause_turn; end-of-turn is the safe subset).
		return llm.StopEndTurn
	case "max_tokens":
		return llm.StopLength
	case "tool_use":
		return llm.StopToolUse
	case "refusal", "sensitive":
		// Refusals must not persist as successful completions.
		return llm.StopError
	case "":
		return ""
	default:
		return llm.StopReason(r)
	}
}

func parseIndex(data string) int {
	var x struct {
		Index int `json:"index"`
	}
	if err := json.Unmarshal([]byte(data), &x); err != nil {
		return -1
	}
	return x.Index
}
