package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"scode/internal/llm"
)

// Canned Messages API SSE stream: text + tool_use + thinking, usage split
// across message_start/message_delta.
const sseFixture = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","role":"assistant","usage":{"input_tokens":100,"cache_creation_input_tokens":50,"cache_read_input_tokens":200}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"bash","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"ls -la\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}

`

func TestStreamAssembly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(sseFixture)) //nolint:errcheck
	}))
	defer srv.Close()

	p := New("test-key", srv.URL)
	tr := buildTestTranscript(t)
	events, err := p.Stream(context.Background(), modelCaps(), tr, llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var got []llm.Event
	for ev := range events {
		got = append(got, ev)
	}
	if err := llm.ValidateStream(got); err != nil {
		t.Fatalf("stream contract: %v", err)
	}

	var final *llm.Message
	for _, ev := range got {
		if ev.Type == llm.EventDone {
			final = ev.Message
		}
	}
	if final == nil {
		t.Fatal("no done event")
	}
	if final.StopReason != llm.StopToolUse {
		t.Fatalf("stopReason = %s", final.StopReason)
	}
	if len(final.Content) != 2 {
		t.Fatalf("content blocks = %d", len(final.Content))
	}
	if final.Content[0].Text != "Hello world" {
		t.Fatalf("text = %q", final.Content[0].Text)
	}
	tc := final.Content[1]
	if tc.Kind != llm.BlockToolCall || tc.ID != "tu_1" || tc.Name != "bash" {
		t.Fatalf("tool call = %+v", tc)
	}
	if string(tc.Arguments) != `{"command":"ls -la"}` {
		t.Fatalf("arguments = %s", tc.Arguments)
	}
	if final.Usage == nil || final.Usage.CacheRead != 200 || final.Usage.CacheWrite != 50 || final.Usage.Output != 42 || final.Usage.Input != 100 {
		t.Fatalf("usage = %+v", final.Usage)
	}
	// Delta events carried the fragments for live display.
	var textDeltas, jsonDeltas []string
	for _, ev := range got {
		if ev.Type == llm.EventTextDelta {
			textDeltas = append(textDeltas, ev.Delta)
		}
		if ev.Type == llm.EventToolCallDelta {
			jsonDeltas = append(jsonDeltas, ev.Delta)
		}
	}
	if strings.Join(textDeltas, "") != "Hello world" {
		t.Fatalf("text deltas = %v", textDeltas)
	}
	if strings.Join(jsonDeltas, "") != `{"command":"ls -la"}` {
		t.Fatalf("json deltas = %v", jsonDeltas)
	}
}

// redacted_thinking must round-trip: opaque data stored on a thinking
// block, replayed verbatim as redacted_thinking.
func TestStreamRedactedThinkingRoundTrip(t *testing.T) {
	asm := newAssembler("claude-x")
	events, _, err := asm.handle("content_block_start", `{"index":0,"content_block":{"type":"redacted_thinking","data":"encrypted-blob=="}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[0].Type != llm.EventThinkingStart {
		t.Fatalf("events = %+v", events)
	}
	events, _, err = asm.handle("message_stop", `{"type":"message_stop"}`)
	if err != nil {
		t.Fatal(err)
	}
	msg := events[len(events)-1].Message
	if len(msg.Content) != 1 || msg.Content[0].Kind != llm.BlockThinking || !msg.Content[0].Redacted || msg.Content[0].Text != "encrypted-blob==" {
		t.Fatalf("message = %+v", msg.Content)
	}
	// Replay shape.
	tr, err := llm.NewTranscript(
		llm.Message{Role: llm.RoleSystem, Content: []llm.Block{llm.TextBlock("s")}},
		llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("hi")}, TS: 1},
		*msg,
		llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("again")}, TS: 2},
	)
	if err != nil {
		t.Fatal(err)
	}
	req, err := BuildRequest(modelCaps(), tr, llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	blk := req.Messages[1].Content[0]
	if blk.Type != "redacted_thinking" || blk.Data != "encrypted-blob==" {
		t.Fatalf("replay = %+v", blk)
	}
}

func TestStreamHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"max_tokens is required"}}`)) //nolint:errcheck
	}))
	defer srv.Close()

	p := New("k", srv.URL)
	events, err := p.Stream(context.Background(), modelCaps(), buildTestTranscript(t), llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var last llm.Event
	for ev := range events {
		last = ev
	}
	if last.Type != llm.EventError {
		t.Fatalf("terminal event = %s, want error", last.Type)
	}
	if last.Message == nil || last.Message.StopReason != llm.StopError || !strings.Contains(last.Message.Error, "max_tokens") {
		t.Fatalf("error message = %+v", last.Message)
	}
}

func TestStreamSSEErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")) //nolint:errcheck
	}))
	defer srv.Close()

	p := New("k", srv.URL)
	events, err := p.Stream(context.Background(), modelCaps(), buildTestTranscript(t), llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var last llm.Event
	for ev := range events {
		last = ev
	}
	if last.Type != llm.EventError || !strings.Contains(last.Message.Error, "overloaded_error") {
		t.Fatalf("last = %+v", last)
	}
}

func TestStreamMissingKey(t *testing.T) {
	p := New("", DefaultBaseURL)
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, err := p.Stream(context.Background(), modelCaps(), buildTestTranscript(t), llm.StreamOptions{}); err == nil {
		t.Fatal("missing key must be a setup error")
	}
}

func TestStreamRetryOn500(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(sseFixture)) //nolint:errcheck
	}))
	defer srv.Close()

	p := New("k", srv.URL)
	start := time.Now()
	events, err := p.Stream(context.Background(), modelCaps(), buildTestTranscript(t), llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var last llm.Event
	for ev := range events {
		last = ev
	}
	if last.Type != llm.EventDone {
		t.Fatalf("terminal = %s after %d calls", last.Type, calls)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	if time.Since(start) < 3*time.Second { // 1s + 2s backoff minimum
		t.Fatalf("retried too fast: %v", time.Since(start))
	}
}

func TestStreamCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{}}}\n\n")) //nolint:errcheck
		<-release
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	p := New("k", srv.URL)
	events, err := p.Stream(ctx, modelCaps(), buildTestTranscript(t), llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	var last llm.Event
	for ev := range events {
		last = ev
	}
	if last.Type != llm.EventError {
		t.Fatalf("terminal = %s, want error", last.Type)
	}
}
