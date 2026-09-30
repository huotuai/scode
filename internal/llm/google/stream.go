package google

import (
	"encoding/json"
	"fmt"

	"scode/internal/llm"
)

// streamPart is the response-side part shape. Text is a pointer because
// pi distinguishes part.text !== undefined from absent (a signature-only
// part opens no block); request-side parts keep the plain struct.
type streamPart struct {
	Text             *string       `json:"text"`
	Thought          bool          `json:"thought"`
	ThoughtSignature string        `json:"thoughtSignature"`
	FunctionCall     *functionCall `json:"functionCall"`
}

// chunk is one GenerateContentResponse SSE payload.
type chunk struct {
	Candidates []struct {
		Content struct {
			Role  string       `json:"role"`
			Parts []streamPart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount        int64 `json:"promptTokenCount"`
		CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
		CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
		ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
		TotalTokenCount         int64 `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

// assembler turns Gemini chunks into scode events and one final assistant
// message (pi's stream loop in google-generative-ai.ts). Text and thinking
// extend the current block until the part kind flips; function calls
// arrive complete and emit start+delta+end atomically.
type assembler struct {
	model string

	blocks     []llm.Block
	current    int // index into blocks of the open text/thinking block, -1 = none
	stopReason llm.StopReason
	rawStop    string
	errText    string
	usage      llm.Usage
}

func newAssembler(model string) *assembler {
	return &assembler{model: model, current: -1}
}

func (a *assembler) snapshot() llm.Message {
	out := llm.Message{
		Role:       llm.RoleAssistant,
		Model:      a.model,
		Provider:   "google",
		Usage:      &a.usage,
		StopReason: a.stopReason,
		Error:      a.errText,
	}
	out.Content = append(out.Content, a.blocks...)
	return out
}

// handleChunk processes one SSE data payload and returns the events to
// emit (pi's per-part state machine).
func (a *assembler) handleChunk(data string) ([]llm.Event, error) {
	var c chunk
	if err := json.Unmarshal([]byte(data), &c); err != nil {
		return nil, fmt.Errorf("google chunk: %w", err)
	}
	var events []llm.Event

	if len(c.Candidates) > 0 {
		cand := c.Candidates[0]
		for _, p := range cand.Content.Parts {
			events = append(events, a.handlePart(p)...)
		}
		if cand.FinishReason != "" {
			a.rawStop = cand.FinishReason
			a.stopReason = mapFinishReason(cand.FinishReason)
			// Tool calls upgrade a plain stop to toolUse (pi).
			if a.stopReason == llm.StopEndTurn {
				for _, b := range a.blocks {
					if b.Kind == llm.BlockToolCall {
						a.stopReason = llm.StopToolUse
						break
					}
				}
			}
		}
	}

	if c.UsageMetadata != nil {
		u := c.UsageMetadata
		a.usage = llm.Usage{
			Input:     u.PromptTokenCount - u.CachedContentTokenCount,
			Output:    u.CandidatesTokenCount + u.ThoughtsTokenCount,
			CacheRead: u.CachedContentTokenCount,
		}
		if a.usage.Input < 0 {
			a.usage.Input = 0
		}
	}
	return events, nil
}

// handlePart processes one content part.
func (a *assembler) handlePart(p streamPart) []llm.Event {
	var events []llm.Event

	// Text-like part (thought or plain): extend/switch the current block.
	// Only parts carrying the text field take this path (pi's part.text
	// !== undefined); a signature-only part is ignored unless it belongs
	// to a function call.
	if p.Text != nil {
		isThinking := p.Thought // pi's isThinkingPart: thought === true
		wantKind := llm.BlockText
		startEv, deltaEv := llm.EventTextStart, llm.EventTextDelta
		if isThinking {
			wantKind = llm.BlockThinking
			startEv, deltaEv = llm.EventThinkingStart, llm.EventThinkingDelta
		}
		if a.current < 0 || a.blocks[a.current].Kind != wantKind {
			events = append(events, a.closeCurrent()...)
			a.blocks = append(a.blocks, llm.Block{Kind: wantKind})
			a.current = len(a.blocks) - 1
			events = append(events, llm.Event{Type: startEv, ContentIndex: a.current})
		}
		cur := &a.blocks[a.current]
		cur.Text += *p.Text
		// Retain the last non-empty signature within the block (pi's
		// retainThoughtSignature).
		if p.ThoughtSignature != "" {
			cur.Signature = p.ThoughtSignature
		}
		events = append(events, llm.Event{Type: deltaEv, ContentIndex: a.current, Delta: *p.Text})
		return events
	}
	if p.FunctionCall == nil {
		return nil
	}

	// Function call part: close the open block, then emit the call
	// atomically (pi pushes start + delta + end).
	events = append(events, a.closeCurrent()...)
	fc := p.FunctionCall
	id := fc.ID
	if id == "" || a.hasToolCallID(id) {
		id = fallbackToolCallID(fc.Name, a.hasToolCallID)
	}
	args := fc.Args
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	call := llm.Block{
		Kind:      llm.BlockToolCall,
		ID:        id,
		Name:      fc.Name,
		Arguments: args,
		Signature: p.ThoughtSignature, // Gemini attaches thought signatures to calls too
	}
	a.blocks = append(a.blocks, call)
	idx := len(a.blocks) - 1
	a.current = -1
	events = append(events,
		llm.Event{Type: llm.EventToolCallStart, ContentIndex: idx},
		llm.Event{Type: llm.EventToolCallDelta, ContentIndex: idx, Delta: string(args)},
		llm.Event{Type: llm.EventToolCallEnd, ContentIndex: idx},
	)
	return events
}

// closeCurrent emits the end event for the open text/thinking block.
func (a *assembler) closeCurrent() []llm.Event {
	if a.current < 0 {
		return nil
	}
	cur := a.blocks[a.current]
	ev := llm.EventTextEnd
	if cur.Kind == llm.BlockThinking {
		ev = llm.EventThinkingEnd
	}
	idx := a.current
	a.current = -1
	return []llm.Event{{Type: ev, ContentIndex: idx}}
}

// finish closes any open block at stream end (pi's post-loop finalization).
func (a *assembler) finish() []llm.Event { return a.closeCurrent() }

func (a *assembler) hasToolCallID(id string) bool {
	for _, b := range a.blocks {
		if b.Kind == llm.BlockToolCall && b.ID == id {
			return true
		}
	}
	return false
}

func (a *assembler) fail(msg string) {
	a.stopReason = llm.StopError
	a.errText = msg
}

func (a *assembler) abort() {
	a.stopReason = llm.StopAborted
	a.errText = "aborted by caller"
}

// mapFinishReason is pi's mapStopReason for Gemini: STOP → stop,
// MAX_TOKENS → length, every other reason (safety, recitation,
// malformed calls, …) → error.
func mapFinishReason(r string) llm.StopReason {
	switch r {
	case "STOP":
		return llm.StopEndTurn
	case "MAX_TOKENS":
		return llm.StopLength
	default:
		return llm.StopError
	}
}
