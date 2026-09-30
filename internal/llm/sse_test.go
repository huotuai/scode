package llm

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadSSEBasic(t *testing.T) {
	fixture := "event: message_start\r\ndata: {\"type\":\"message_start\"}\r\n\r\n" +
		": keep-alive comment\n\n" +
		"data: line one\n" +
		"data: line two\n\n" +
		"event: done\ndata: [DONE]\n\n"
	var got []SSEEvent
	err := ReadSSE(strings.NewReader(fixture), func(e SSEEvent) error {
		got = append(got, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []SSEEvent{
		{Name: "message_start", Data: `{"type":"message_start"}`},
		{Name: "", Data: "line one\nline two"},
		{Name: "done", Data: "[DONE]"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestReadSSEFinalEventWithoutBlankLine(t *testing.T) {
	var got []SSEEvent
	err := ReadSSE(strings.NewReader("data: tail"), func(e SSEEvent) error {
		got = append(got, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Data != "tail" {
		t.Fatalf("got %+v", got)
	}
}

func TestReadSSEDataPrefixWithoutSpace(t *testing.T) {
	// "data:nospace" (no space after colon) is legal and yields "nospace".
	var got string
	err := ReadSSE(strings.NewReader("data:nospace\n\n"), func(e SSEEvent) error {
		got = e.Data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "nospace" {
		t.Fatalf("data = %q", got)
	}
}

func TestReadSSEStopError(t *testing.T) {
	sentinel := errors.New("stop")
	calls := 0
	err := ReadSSE(strings.NewReader("data: a\n\ndata: b\n\ndata: c\n\n"), func(e SSEEvent) error {
		calls++
		if calls == 2 {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if calls != 2 {
		t.Fatalf("callback ran %d times, want 2", calls)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// A mid-body transport drop (io.ErrUnexpectedEOF from the HTTP layer)
// must surface with pi's truncation wording — the agent retry whitelist
// keys off it — while the Go cause stays unwrappable.
func TestReadSSETransportDropWording(t *testing.T) {
	err := ReadSSE(errReader{io.ErrUnexpectedEOF}, func(SSEEvent) error { return nil })
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want unexpected EOF cause", err)
	}
	if !strings.Contains(err.Error(), "stream ended before a terminal event") {
		t.Fatalf("err = %q, want pi truncation wording", err)
	}
}
