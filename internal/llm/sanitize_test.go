package llm

import (
	"encoding/json"
	"testing"
)

// NormalizeArguments guarantees tool-call arguments are always
// marshalable: invalid fragments are wrapped, empty becomes {}, valid
// JSON is untouched.
func TestNormalizeArguments(t *testing.T) {
	m := Message{Role: RoleAssistant, Content: []Block{
		{Kind: BlockToolCall, Name: "bash", Arguments: json.RawMessage(`{"command":oops`)},
		{Kind: BlockToolCall, Name: "read", Arguments: nil},
		{Kind: BlockToolCall, Name: "write", Arguments: json.RawMessage(`{"path":"a.txt"}`)},
		{Kind: BlockText, Text: "unchanged"},
	}}
	NormalizeArguments(&m)

	for i, b := range m.Content {
		if b.Kind == BlockToolCall && !json.Valid(b.Arguments) {
			t.Fatalf("block %d left invalid: %q", i, b.Arguments)
		}
	}
	if got := string(m.Content[0].Arguments); got != `"{\"command\":oops"` {
		t.Fatalf("invalid fragment should wrap as a JSON string, got %s", got)
	}
	if got := string(m.Content[1].Arguments); got != `{}` {
		t.Fatalf("empty arguments should become {}, got %s", got)
	}
	if got := string(m.Content[2].Arguments); got != `{"path":"a.txt"}` {
		t.Fatalf("valid arguments must be untouched, got %s", got)
	}
	if _, err := json.Marshal(m); err != nil {
		t.Fatalf("normalized message must marshal: %v", err)
	}
}
