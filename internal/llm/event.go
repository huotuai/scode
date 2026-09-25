package llm

import "fmt"

// EventType enumerates the streaming lifecycle events a provider emits while
// assembling one assistant message (pi's AssistantMessageEvent contract).
//
// A stream always begins with Start, interleaves block lifecycles
// (TextStart/Delta/End, ThinkingStart/Delta/End, ToolCallStart/Delta/End in
// any order and interleaved), and terminates with exactly one of Done or
// Error. Failures are encoded in-stream rather than returned as errors from
// Stream: the terminal Error event carries an assistant message with
// StopReason error|aborted so the agent loop can persist the failure like
// any other turn (pi's StreamFn contract — "must not throw").
type EventType string

const (
	EventStart         EventType = "start"
	EventTextStart     EventType = "text_start"
	EventTextDelta     EventType = "text_delta"
	EventTextEnd       EventType = "text_end"
	EventThinkingStart EventType = "thinking_start"
	EventThinkingDelta EventType = "thinking_delta"
	EventThinkingEnd   EventType = "thinking_end"
	EventToolCallStart EventType = "toolcall_start"
	EventToolCallDelta EventType = "toolcall_delta"
	EventToolCallEnd   EventType = "toolcall_end"
	EventDone          EventType = "done"
	EventError         EventType = "error"
)

// Event is one streaming event. Which fields are meaningful depends on Type:
//
//   - Start/Done/Error: Message carries the (partial or final) assistant
//     message; Reason is set for Done ("stop"|"length"|"toolUse") and Error
//     ("aborted"|"error").
//   - *Start/*End: ContentIndex identifies the block within Message.Content.
//   - *Delta: Delta carries the appended fragment — decoded text for
//     text/thinking, raw JSON-string fragment for tool arguments.
type Event struct {
	Type         EventType
	ContentIndex int
	Delta        string
	Reason       StopReason
	Message      *Message
	Err          error
}

// Terminal reports whether the event ends the stream (Done or Error).
func (e Event) Terminal() bool { return e.Type == EventDone || e.Type == EventError }

// Validate checks the stream contract described on EventType. It exists for
// provider tests; production consumers switch on Type directly.
func ValidateStream(events []Event) error {
	if len(events) == 0 {
		return fmt.Errorf("empty stream")
	}
	if events[0].Type != EventStart {
		return fmt.Errorf("first event is %s, want start", events[0].Type)
	}
	if events[0].Message == nil {
		return fmt.Errorf("start event without message")
	}
	terminals := 0
	for i, e := range events[1:] {
		if e.Terminal() {
			terminals++
			if terminals > 1 {
				return fmt.Errorf("event %d: second terminal event", i+1)
			}
			if e.Message == nil {
				return fmt.Errorf("event %d: terminal event without message", i+1)
			}
			continue
		}
		if terminals > 0 {
			return fmt.Errorf("event %d: event after terminal", i+1)
		}
	}
	if terminals != 1 {
		return fmt.Errorf("stream has no terminal event")
	}
	return nil
}
