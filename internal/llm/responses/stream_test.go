package responses

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"scode/internal/llm"
)

// sse writes one named SSE event per payload.
func sse(payloads ...string) string {
	var sb strings.Builder
	for _, p := range payloads {
		var parsed struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(p), &parsed)
		fmt.Fprintf(&sb, "event: %s\ndata: %s\n\n", parsed.Type, p)
	}
	return sb.String()
}

// runStream feeds payloads through a Provider's full HTTP path.
func runStream(t *testing.T, p *OpenAIProvider, body string, model llm.Model, tr *llm.Transcript) []llm.Event {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	p.BaseURL = srv.URL
	ch, err := p.Stream(context.Background(), model, tr, llm.StreamOptions{APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	var events []llm.Event
	for e := range ch {
		events = append(events, e)
	}
	return events
}

func TestStreamTextFlow(t *testing.T) {
	body := sse(
		`{"type":"response.created","response":{"id":"resp_1"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[],"status":"in_progress"}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"Hello"}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":" world"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Hello world","annotations":[]}],"status":"completed"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":12,"output_tokens":5,"total_tokens":17,"input_tokens_details":{"cached_tokens":10}}}}`,
	)
	events := runStream(t, NewOpenAI("k", ""), body, model(), testTranscript(t))
	if err := llm.ValidateStream(events); err != nil {
		t.Fatalf("stream contract: %v", err)
	}
	last := events[len(events)-1]
	if last.Type != llm.EventDone || last.Reason != llm.StopEndTurn {
		t.Fatalf("terminal = %+v", last)
	}
	msg := last.Message
	if len(msg.Content) != 1 || msg.Content[0].Text != "Hello world" {
		t.Fatalf("content = %+v", msg.Content)
	}
	// The text signature carries the message item id for replay.
	var sig textSignatureV1
	if err := json.Unmarshal([]byte(msg.Content[0].Signature), &sig); err != nil || sig.ID != "msg_1" || sig.V != 1 {
		t.Fatalf("text signature = %q", msg.Content[0].Signature)
	}
	// Usage: cached tokens subtracted from input (pi).
	if msg.Usage.Input != 2 || msg.Usage.CacheRead != 10 || msg.Usage.Output != 5 {
		t.Fatalf("usage = %+v", msg.Usage)
	}
}

func TestStreamToolCallFlow(t *testing.T) {
	body := sse(
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"bash","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"command\":"}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"ls\"}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"bash","arguments":"{\"command\":\"ls\"}"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`,
	)
	events := runStream(t, NewOpenAI("k", ""), body, model(), testTranscript(t))
	if err := llm.ValidateStream(events); err != nil {
		t.Fatalf("stream contract: %v", err)
	}
	last := events[len(events)-1]
	// Tool calls upgrade stop to toolUse (pi).
	if last.Type != llm.EventDone || last.Reason != llm.StopToolUse {
		t.Fatalf("terminal = %+v", last)
	}
	call := last.Message.Content[0]
	if call.Kind != llm.BlockToolCall || call.ID != "call_1|fc_1" || call.Name != "bash" {
		t.Fatalf("tool call = %+v", call)
	}
	if string(call.Arguments) != `{"command":"ls"}` {
		t.Fatalf("arguments = %s", call.Arguments)
	}
}

func TestStreamReasoningAndBackfill(t *testing.T) {
	// Azure-style: encrypted_content arrives only in the terminal
	// response's output, not in output_item.done (pi's backfill).
	body := sse(
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"thinking hard"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"thinking hard"}]}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_2","role":"assistant","content":[],"status":"in_progress"}}`,
		`{"type":"response.output_text.delta","output_index":1,"delta":"done"}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg_2","role":"assistant","content":[{"type":"output_text","text":"done","annotations":[]}],"status":"completed","phase":"final_answer"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"thinking hard"}],"encrypted_content":"ENC123"}]}}`,
	)
	events := runStream(t, NewOpenAI("k", ""), body, model(), testTranscript(t))
	last := events[len(events)-1]
	if last.Type != llm.EventDone || last.Reason != llm.StopEndTurn {
		t.Fatalf("terminal = %+v", last)
	}
	msg := last.Message
	if len(msg.Content) != 2 {
		t.Fatalf("content = %+v", msg.Content)
	}
	th := msg.Content[0]
	if th.Kind != llm.BlockThinking || th.Text != "thinking hard" {
		t.Fatalf("thinking = %+v", th)
	}
	// Signature = the verbatim reasoning item JSON with encrypted_content
	// backfilled from the terminal response.
	var sigItem map[string]any
	if err := json.Unmarshal([]byte(th.Signature), &sigItem); err != nil {
		t.Fatalf("signature not JSON: %q", th.Signature)
	}
	if sigItem["id"] != "rs_1" || sigItem["encrypted_content"] != "ENC123" {
		t.Fatalf("backfilled signature = %s", th.Signature)
	}
}

func TestStreamIncompleteMaxTokens(t *testing.T) {
	body := sse(
		`{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`,
	)
	events := runStream(t, NewOpenAI("k", ""), body, model(), testTranscript(t))
	last := events[len(events)-1]
	if last.Type != llm.EventDone || last.Reason != llm.StopLength {
		t.Fatalf("terminal = %+v", last)
	}
}

func TestStreamFailed(t *testing.T) {
	body := sse(
		`{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"server_error","message":"boom"}}}`,
	)
	events := runStream(t, NewOpenAI("k", ""), body, model(), testTranscript(t))
	last := events[len(events)-1]
	if last.Type != llm.EventError || last.Reason != llm.StopError {
		t.Fatalf("terminal = %+v", last)
	}
	if last.Message.Error != "server_error: boom" {
		t.Fatalf("error = %q", last.Message.Error)
	}
}

func TestStreamPrematureEOF(t *testing.T) {
	body := sse(`{"type":"response.created","response":{"id":"resp_1"}}`)
	events := runStream(t, NewOpenAI("k", ""), body, model(), testTranscript(t))
	last := events[len(events)-1]
	if last.Type != llm.EventError || last.Message.StopReason != llm.StopError {
		t.Fatalf("terminal = %+v", last)
	}
	if !strings.Contains(last.Message.Error, "terminal") {
		t.Fatalf("error = %q", last.Message.Error)
	}
}

func TestStreamHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"type":"invalid_request_error","message":"bad input"}}`)
	}))
	t.Cleanup(srv.Close)
	p := NewOpenAI("k", srv.URL)
	ch, err := p.Stream(context.Background(), model(), testTranscript(t), llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var last llm.Event
	for e := range ch {
		last = e
	}
	if last.Type != llm.EventError || !strings.Contains(last.Message.Error, "invalid_request_error: bad input") {
		t.Fatalf("error event = %+v", last)
	}
}
