package llm

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// SSEEvent is one parsed server-sent event: the last `event:` field value
// (empty means the default "message" event) and the `data:` lines joined
// with newlines.
type SSEEvent struct {
	Name string
	Data string
}

// maxSSELine caps a single SSE line. Deltas can be large (whole tool-call
// arguments in one burst), so this is generous but not unbounded.
const maxSSELine = 16 << 20 // 16 MiB

// ReadSSE parses a text/event-stream body and invokes fn once per complete
// event. It returns nil on EOF. Per the SSE spec: `event:` names the event,
// consecutive `data:` lines concatenate with newlines, lines starting with
// `:` are comments, a blank line dispatches the accumulated event, and CR
// is stripped from CRLF line endings. Multi-byte field values after the
// colon are skipped by one optional space.
//
// The HTTP response body owns cancellation: closing it unblocks the read,
// so callers wire context cancellation to resp.Body.Close and treat the
// resulting error as abort.
func ReadSSE(r io.Reader, fn func(SSEEvent) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxSSELine)

	var name string
	var data []string
	flush := func() error {
		if name == "" && len(data) == 0 {
			return nil
		}
		ev := SSEEvent{Name: name, Data: strings.Join(data, "\n")}
		name, data = "", nil
		return fn(ev)
	}

	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, ":"):
			// comment / keep-alive
		case strings.HasPrefix(line, "event:"):
			name = trimField(line, "event:")
		case strings.HasPrefix(line, "data:"):
			data = append(data, trimField(line, "data:"))
		default:
			// id:, retry:, and unknown fields are ignored.
		}
	}
	if err := sc.Err(); err != nil {
		// The body died mid-stream (unexpected EOF, reset, ...): carry
		// pi's truncation wording so the agent retry whitelist (pi's
		// retry.ts patterns) recognizes transport drops; the Go cause
		// rides along for surfacing.
		return fmt.Errorf("stream ended before a terminal event: sse: %w", err)
	}
	// A final event without a trailing blank line still dispatches.
	return flush()
}

// trimField strips the field prefix and at most one leading space.
func trimField(line, field string) string {
	v := strings.TrimPrefix(line, field)
	return strings.TrimPrefix(v, " ")
}
