package responses

import (
	"encoding/json"
	"fmt"
	"strings"

	"scode/internal/llm"
)

// ---------------------------------------------------------------------------
// SSE event shapes (data JSON always carries "type"; the SSE event name
// duplicates it)
// ---------------------------------------------------------------------------

type streamEvent struct {
	Type        string          `json:"type"`
	OutputIndex int             `json:"output_index"`
	Delta       string          `json:"delta"`
	Arguments   string          `json:"arguments"` // response.function_call_arguments.done
	Item        json.RawMessage `json:"item"`
	Response    json.RawMessage `json:"response"`
	Code        string          `json:"code"`    // error event
	Message     string          `json:"message"` // error event
}

// outputItem is the union of reasoning / message / function_call output
// items. Raw keeps the full item JSON for thinking signatures (replay is
// verbatim; unknown fields must survive).
type outputItem struct {
	Raw       json.RawMessage `json:"-"`
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"` // function_call (JSON-encoded string)
	Status    string          `json:"status"`
	Phase     string          `json:"phase"`
	Role      string          `json:"role"`
	Content   []struct {
		Type    string `json:"type"` // output_text | refusal | reasoning_text
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	Summary []struct {
		Type string `json:"type"` // summary_text
		Text string `json:"text"`
	} `json:"summary"`
	EncryptedContent string `json:"encrypted_content"`
}

func (it *outputItem) unmarshal(raw json.RawMessage) error {
	it.Raw = raw
	return json.Unmarshal(raw, it)
}

type responseObject struct {
	ID     string            `json:"id"`
	Status string            `json:"status"`
	Output []json.RawMessage `json:"output"`
	Usage  *struct {
		InputTokens        int64 `json:"input_tokens"`
		OutputTokens       int64 `json:"output_tokens"`
		TotalTokens        int64 `json:"total_tokens"`
		InputTokensDetails struct {
			CachedTokens     int64 `json:"cached_tokens"`
			CacheWriteTokens int64 `json:"cache_write_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ---------------------------------------------------------------------------
// Assembler (pi's processResponsesStream)
// ---------------------------------------------------------------------------

// slot is one output slot under assembly.
type slot struct {
	kind         llm.BlockKind // BlockThinking | BlockText | BlockToolCall
	block        llm.Block
	contentIndex int
	partialJSON  string // toolCall argument fragments
}

// assembler converts Responses stream events into scode events and one
// final assistant message. Slots leave the live map at item.done (pi
// deletes the output slot) but stay in the final message via finalized.
type assembler struct {
	model    string
	provider string

	slots         map[int]*slot    // output_index → live slot
	finalized     []*slot          // done slots, still part of the message
	reasoningByID map[string]*slot // reasoning item id → slot (encrypted-content backfill)
	nblocks       int
	stopReason    llm.StopReason
	stopErr       string
	rawStop       string
	usage         llm.Usage
	sawTerminal   bool
}

func newAssembler(model, provider string) *assembler {
	return &assembler{
		model:         model,
		provider:      provider,
		slots:         map[int]*slot{},
		reasoningByID: map[string]*slot{},
	}
}

// snapshot renders the assistant message accumulated so far.
func (a *assembler) snapshot() llm.Message {
	out := llm.Message{
		Role:       llm.RoleAssistant,
		Model:      a.model,
		Provider:   a.provider,
		Usage:      &a.usage,
		StopReason: a.stopReason,
		Error:      a.stopErr,
	}
	for _, s := range a.orderedSlots() {
		b := s.block
		if b.Kind == llm.BlockToolCall {
			b.Arguments = compactArgsRaw(s.partialJSON, b.Arguments)
		}
		out.Content = append(out.Content, b)
	}
	return out
}

func (a *assembler) orderedSlots() []*slot {
	all := a.allBlocks()
	for i := 1; i < len(all); i++ { // insertion sort: content order is arrival order
		for j := i; j > 0 && all[j].contentIndex < all[j-1].contentIndex; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	return all
}

// compactArgsRaw prefers the server-final arguments string; otherwise
// normalizes accumulated fragments ({} fallback keeps the persisted
// message replayable — pi's parseStreamingJson guarantees an object).
func compactArgsRaw(partial string, final json.RawMessage) json.RawMessage {
	s := strings.TrimSpace(string(final))
	if s == "" {
		s = strings.TrimSpace(partial)
	}
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

// createSlot handles response.output_item.added.
func (a *assembler) createSlot(outputIndex int, item *outputItem) []llm.Event {
	switch item.Type {
	case "reasoning":
		s := &slot{kind: llm.BlockThinking, block: llm.Block{Kind: llm.BlockThinking}, contentIndex: a.nblocks}
		a.nblocks++
		a.slots[outputIndex] = s
		return []llm.Event{{Type: llm.EventThinkingStart, ContentIndex: s.contentIndex}}
	case "message":
		a.applyMessagePhase(item)
		s := &slot{kind: llm.BlockText, block: llm.Block{Kind: llm.BlockText}, contentIndex: a.nblocks}
		a.nblocks++
		a.slots[outputIndex] = s
		return []llm.Event{{Type: llm.EventTextStart, ContentIndex: s.contentIndex}}
	case "function_call":
		s := &slot{
			kind: llm.BlockToolCall,
			block: llm.Block{
				Kind: llm.BlockToolCall,
				ID:   item.CallID + "|" + item.ID,
				Name: item.Name,
			},
			contentIndex: a.nblocks,
			partialJSON:  item.Arguments,
		}
		a.nblocks++
		a.slots[outputIndex] = s
		return []llm.Event{{Type: llm.EventToolCallStart, ContentIndex: s.contentIndex}}
	}
	return nil // custom_tool_call and unknown items: unreachable (no grammar tools declared)
}

// applyMessagePhase: a final_answer phase pins the stop reason to stop
// (pi's applyMessagePhaseStopReason).
func (a *assembler) applyMessagePhase(item *outputItem) {
	if item.Type == "message" && item.Phase == "final_answer" {
		a.stopReason = llm.StopEndTurn
	}
}

// handle processes one SSE data payload. It returns the events to emit;
// terminal is true after response.completed/incomplete/failed or error.
func (a *assembler) handle(data string) (events []llm.Event, terminal bool, err error) {
	var ev streamEvent
	if uerr := json.Unmarshal([]byte(data), &ev); uerr != nil {
		return nil, false, fmt.Errorf("responses event: %w", uerr)
	}

	switch ev.Type {
	case "response.created":
		return nil, false, nil // responseId has no slot in scode's message model

	case "response.output_item.added":
		var item outputItem
		if uerr := item.unmarshal(ev.Item); uerr != nil {
			return nil, false, fmt.Errorf("output_item.added: %w", uerr)
		}
		return a.createSlot(ev.OutputIndex, &item), false, nil

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		s := a.getSlot(ev.OutputIndex, llm.BlockThinking)
		if s == nil {
			return nil, false, nil
		}
		s.block.Text += ev.Delta
		return []llm.Event{{Type: llm.EventThinkingDelta, ContentIndex: s.contentIndex, Delta: ev.Delta}}, false, nil

	case "response.reasoning_summary_part.done":
		s := a.getSlot(ev.OutputIndex, llm.BlockThinking)
		if s == nil {
			return nil, false, nil
		}
		s.block.Text += "\n\n"
		return []llm.Event{{Type: llm.EventThinkingDelta, ContentIndex: s.contentIndex, Delta: "\n\n"}}, false, nil

	case "response.output_text.delta", "response.refusal.delta":
		s := a.getSlot(ev.OutputIndex, llm.BlockText)
		if s == nil {
			return nil, false, nil
		}
		s.block.Text += ev.Delta
		return []llm.Event{{Type: llm.EventTextDelta, ContentIndex: s.contentIndex, Delta: ev.Delta}}, false, nil

	case "response.function_call_arguments.delta":
		s := a.getSlot(ev.OutputIndex, llm.BlockToolCall)
		if s == nil {
			return nil, false, nil
		}
		s.partialJSON += ev.Delta
		return []llm.Event{{Type: llm.EventToolCallDelta, ContentIndex: s.contentIndex, Delta: ev.Delta}}, false, nil

	case "response.function_call_arguments.done":
		s := a.getSlot(ev.OutputIndex, llm.BlockToolCall)
		if s == nil {
			return nil, false, nil
		}
		prev := s.partialJSON
		s.partialJSON = ev.Arguments
		s.block.Arguments = json.RawMessage(ev.Arguments)
		// Emit the suffix as a delta when the final string extends what
		// was streamed (pi's done-event delta rule).
		if strings.HasPrefix(ev.Arguments, prev) && len(ev.Arguments) > len(prev) {
			return []llm.Event{{Type: llm.EventToolCallDelta, ContentIndex: s.contentIndex, Delta: ev.Arguments[len(prev):]}}, false, nil
		}
		return nil, false, nil

	case "response.output_item.done":
		return a.itemDone(ev.OutputIndex, ev.Item)

	case "response.completed", "response.incomplete":
		var resp responseObject
		if uerr := json.Unmarshal(ev.Response, &resp); uerr != nil {
			return nil, false, fmt.Errorf("response.completed: %w", uerr)
		}
		a.finalizeResponse(&resp)
		msg := a.snapshot()
		if a.stopReason == llm.StopError {
			return []llm.Event{{Type: llm.EventError, Message: &msg, Reason: llm.StopError, Err: fmt.Errorf("%s", a.stopErr)}}, true, nil
		}
		return []llm.Event{{Type: llm.EventDone, Message: &msg, Reason: a.stopReason}}, true, nil

	case "response.failed":
		a.sawTerminal = true
		var resp responseObject
		_ = json.Unmarshal(ev.Response, &resp)
		msg := "Unknown error (no error details in response)"
		switch {
		case resp.Error != nil:
			code := resp.Error.Code
			if code == "" {
				code = "unknown"
			}
			m := resp.Error.Message
			if m == "" {
				m = "no message"
			}
			msg = code + ": " + m
		case resp.IncompleteDetails != nil && resp.IncompleteDetails.Reason != "":
			msg = "incomplete: " + resp.IncompleteDetails.Reason
		}
		a.stopReason = llm.StopError
		a.stopErr = msg
		snap := a.snapshot()
		return []llm.Event{{Type: llm.EventError, Message: &snap, Reason: llm.StopError, Err: fmt.Errorf("%s", msg)}}, true, nil

	case "error":
		msg := fmt.Sprintf("Error Code %s: %s", ev.Code, ev.Message)
		a.stopReason = llm.StopError
		a.stopErr = msg
		snap := a.snapshot()
		return []llm.Event{{Type: llm.EventError, Message: &snap, Reason: llm.StopError, Err: fmt.Errorf("%s", msg)}}, true, nil
	}
	return nil, false, nil
}

// itemDone finalizes one output slot (pi's response.output_item.done).
func (a *assembler) itemDone(outputIndex int, raw json.RawMessage) ([]llm.Event, bool, error) {
	var item outputItem
	if err := item.unmarshal(raw); err != nil {
		return nil, false, fmt.Errorf("output_item.done: %w", err)
	}
	a.applyMessagePhase(&item)
	// A done can arrive without a prior added (pi's getOrCreateSlot);
	// emit the synthesized start events before the end.
	var prefix []llm.Event
	s := a.slots[outputIndex]
	if s == nil {
		prefix = a.createSlot(outputIndex, &item)
		s = a.slots[outputIndex]
	}
	if s == nil {
		return nil, false, nil
	}

	switch {
	case item.Type == "reasoning" && s.kind == llm.BlockThinking:
		var parts []string
		for _, p := range item.Summary {
			parts = append(parts, p.Text)
		}
		summary := strings.Join(parts, "\n\n")
		var cparts []string
		for _, c := range item.Content {
			cparts = append(cparts, c.Text)
		}
		content := strings.Join(cparts, "\n\n")
		if summary != "" {
			s.block.Text = summary
		} else if content != "" {
			s.block.Text = content
		}
		// The signature is the item's verbatim JSON (stateless replay).
		s.block.Signature = string(item.Raw)
		a.reasoningByID[item.ID] = s
		a.finalize(s)
		return append(prefix, llm.Event{Type: llm.EventThinkingEnd, ContentIndex: s.contentIndex}), false, nil

	case item.Type == "message" && s.kind == llm.BlockText:
		var sb strings.Builder
		for _, c := range item.Content {
			if c.Type == "output_text" {
				sb.WriteString(c.Text)
			} else {
				sb.WriteString(c.Refusal)
			}
		}
		s.block.Text = sb.String()
		s.block.Signature = encodeTextSignature(item.ID, item.Phase)
		a.finalize(s)
		return append(prefix, llm.Event{Type: llm.EventTextEnd, ContentIndex: s.contentIndex}), false, nil

	case item.Type == "function_call" && s.kind == llm.BlockToolCall:
		if item.Arguments != "" {
			s.partialJSON = item.Arguments
			s.block.Arguments = json.RawMessage(item.Arguments)
		}
		a.finalize(s)
		return append(prefix, llm.Event{Type: llm.EventToolCallEnd, ContentIndex: s.contentIndex}), false, nil
	}
	return nil, false, nil
}

// finalizeResponse handles response.completed/incomplete (pi's
// finalizeResponse): usage, reasoning-signature backfill, stop mapping.
func (a *assembler) finalizeResponse(resp *responseObject) {
	a.sawTerminal = true
	a.backfillReasoningSignatures(resp.Output)
	if resp.Usage != nil {
		cached := resp.Usage.InputTokensDetails.CachedTokens
		write := resp.Usage.InputTokensDetails.CacheWriteTokens
		// OpenAI folds cached and cache-write tokens into input_tokens;
		// subtract both (pi).
		input := resp.Usage.InputTokens - cached - write
		if input < 0 {
			input = 0
		}
		a.usage = llm.Usage{
			Input:      input,
			Output:     resp.Usage.OutputTokens,
			CacheRead:  cached,
			CacheWrite: write,
		}
	}
	incompleteReason := ""
	if resp.IncompleteDetails != nil {
		incompleteReason = resp.IncompleteDetails.Reason
	}
	a.rawStop = resp.Status
	if incompleteReason != "" {
		a.rawStop = resp.Status + "." + incompleteReason
	}
	a.stopReason, a.stopErr = mapStopReason(resp.Status, incompleteReason)
	// Tool calls upgrade a plain stop to toolUse (pi).
	if a.stopReason == llm.StopEndTurn {
		for _, s := range a.allBlocks() {
			if s.block.Kind == llm.BlockToolCall {
				a.stopReason = llm.StopToolUse
				break
			}
		}
	}
}

// backfillReasoningSignatures merges encrypted_content delivered only in
// the terminal response into the stored reasoning items (pi's Azure
// workaround, issue #6409).
func (a *assembler) backfillReasoningSignatures(output []json.RawMessage) {
	for _, raw := range output {
		var item outputItem
		if err := item.unmarshal(raw); err != nil {
			continue
		}
		if item.Type != "reasoning" || item.EncryptedContent == "" {
			continue
		}
		s := a.reasoningByID[item.ID]
		if s == nil || s.block.Signature == "" {
			continue
		}
		var stored map[string]any
		if err := json.Unmarshal([]byte(s.block.Signature), &stored); err != nil {
			continue
		}
		if ec, _ := stored["encrypted_content"].(string); ec != "" {
			continue
		}
		stored["encrypted_content"] = item.EncryptedContent
		if b, err := json.Marshal(stored); err == nil {
			s.block.Signature = string(b)
		}
	}
}

// mapStopReason mirrors pi's OpenAI Responses stop mapping.
func mapStopReason(status, incompleteReason string) (llm.StopReason, string) {
	switch status {
	case "", "completed", "in_progress", "queued":
		return llm.StopEndTurn, ""
	case "incomplete":
		if incompleteReason == "max_output_tokens" {
			return llm.StopLength, ""
		}
		if incompleteReason != "" {
			return llm.StopError, "Response incomplete: " + incompleteReason
		}
		return llm.StopError, "Response incomplete without a provider reason"
	case "failed", "cancelled":
		return llm.StopError, ""
	default:
		// pi throws on unhandled statuses; surface as an error turn.
		return llm.StopError, "Unhandled stop reason: " + status
	}
}

func (a *assembler) getSlot(outputIndex int, kind llm.BlockKind) *slot {
	s := a.slots[outputIndex]
	if s == nil || s.kind != kind {
		return nil
	}
	return s
}

// finalize moves a slot out of the live map into the finalized list so
// the final message keeps every block (pi deletes the output slot at
// item.done while the content array keeps the block).
func (a *assembler) finalize(s *slot) {
	for i, cur := range a.slots {
		if cur == s {
			delete(a.slots, i)
			break
		}
	}
	a.finalized = append(a.finalized, s)
}

func (a *assembler) allBlocks() []*slot {
	out := append([]*slot{}, a.finalized...)
	for _, s := range a.slots {
		out = append(out, s)
	}
	return out
}
