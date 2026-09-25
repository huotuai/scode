package openaic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"scode/internal/llm"
)

func testTranscript(t *testing.T) *llm.Transcript {
	t.Helper()
	tr, err := llm.NormalizeContext(llm.Context{
		SystemPrompt: "You are scode.",
		Tools: []llm.Tool{
			{Name: "bash", Description: "run a command", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)},
		},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("list files")}, TS: 1},
			{
				Role: llm.RoleAssistant, TS: 2, StopReason: llm.StopToolUse,
				Content: []llm.Block{
					llm.Block{Kind: llm.BlockToolCall, ID: "call_1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)},
				},
			},
			{
				Role: llm.RoleTool, TS: 3,
				Content: []llm.Block{
					{Kind: llm.BlockToolResult, ID: "call_1", Content: []llm.Block{llm.TextBlock("a.txt")}},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func model() llm.Model {
	return llm.Model{ID: "gpt-x", Provider: "openai-compat", APIShape: "openai-completions"}
}

func TestBuildRequestShape(t *testing.T) {
	req, err := BuildRequest(model(), testTranscript(t), llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 4 {
		t.Fatalf("messages = %d, want 4 (system,user,assistant,tool)", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "You are scode." {
		t.Fatalf("system = %+v", req.Messages[0])
	}
	asst := req.Messages[2]
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "call_1" {
		t.Fatalf("assistant tool_calls = %+v", asst.ToolCalls)
	}
	if fn := asst.ToolCalls[0].Function; fn.Name != "bash" || string(mustJSON(fn.Parameters)) != `{"command":"ls"}` {
		t.Fatalf("function = %+v", fn)
	}
	tool := req.Messages[3]
	if tool.Role != "tool" || tool.ToolCallID != "call_1" || tool.Content != "a.txt" {
		t.Fatalf("tool message = %+v", tool)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "bash" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
		t.Fatal("stream_options.include_usage must be set")
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestBuildRequestPromptCacheKey(t *testing.T) {
	long := strings.Repeat("s", 100)
	req, err := BuildRequest(model(), testTranscript(t), llm.StreamOptions{PromptCacheKey: long})
	if err != nil {
		t.Fatal(err)
	}
	if len(req.PromptCacheKey) != 64 {
		t.Fatalf("prompt_cache_key len = %d, want clamped 64", len(req.PromptCacheKey))
	}
	req2, err := BuildRequest(model(), testTranscript(t), llm.StreamOptions{PromptCacheKey: "sess-1", Cache: llm.CacheNone})
	if err != nil {
		t.Fatal(err)
	}
	if req2.PromptCacheKey != "" {
		t.Fatal("cache none must drop prompt_cache_key (one-off calls)")
	}
}

func TestBuildRequestToolErrorPrefix(t *testing.T) {
	tr, err := llm.NormalizeContext(llm.Context{
		SystemPrompt: "p",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("go")}, TS: 1},
			{Role: llm.RoleAssistant, TS: 2, Content: []llm.Block{llm.ToolCallBlock("c1", "bash")}},
			{Role: llm.RoleTool, TS: 3, Content: []llm.Block{
				{Kind: llm.BlockToolResult, ID: "c1", IsError: true, Content: []llm.Block{llm.TextBlock("boom")}},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := BuildRequest(model(), tr, llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if req.Messages[3].Content != "ERROR: boom" {
		t.Fatalf("error tool content = %q", req.Messages[3].Content)
	}
}

// Prefix stability across turns: identical system/tools/message-prefix
// bytes (implicit cache requirement).
func TestBuildRequestPrefixStability(t *testing.T) {
	tr := testTranscript(t)
	req1, err := BuildRequest(model(), tr, llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(req1.Messages)
	if err := tr.Append(llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{llm.TextBlock("done")}, StopReason: llm.StopEndTurn, TS: 9}); err != nil {
		t.Fatal(err)
	}
	req2, err := BuildRequest(model(), tr, llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(req2.Messages[:len(req1.Messages)])
	if string(before) != string(after) {
		t.Fatalf("message prefix changed:\n%s\n%s", before, after)
	}
}

const chatSSE = `data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}

data: {"choices":[{"index":0,"delta":{"content":"Hel"}}]}

data: {"choices":[{"index":0,"delta":{"content":"lo"}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"bash","arguments":"{\"comm"}}]}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"and\":\"pwd\"}"}}]}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":500,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":400}}}

data: [DONE]

`

func TestStreamAssembly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") == "" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(chatSSE)) //nolint:errcheck
	}))
	defer srv.Close()

	p := New("k", srv.URL)
	events, err := p.Stream(context.Background(), model(), testTranscript(t), llm.StreamOptions{})
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
	if len(final.Content) != 2 || final.Content[0].Text != "Hello" {
		t.Fatalf("content = %+v", final.Content)
	}
	tc := final.Content[1]
	if tc.Name != "bash" || tc.ID != "call_9" || string(tc.Arguments) != `{"command":"pwd"}` {
		t.Fatalf("tool call = %+v args=%s", tc, tc.Arguments)
	}
	if final.Usage.CacheRead != 400 || final.Usage.Input != 100 || final.Usage.Output != 20 {
		t.Fatalf("usage = %+v", final.Usage)
	}
}

func TestStreamHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":{"message":"Incorrect API key","type":"invalid_request_error"}}`)) //nolint:errcheck
	}))
	defer srv.Close()
	p := New("k", srv.URL)
	events, err := p.Stream(context.Background(), model(), testTranscript(t), llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var last llm.Event
	for ev := range events {
		last = ev
	}
	if last.Type != llm.EventError || !strings.Contains(last.Message.Error, "Incorrect API key") {
		t.Fatalf("last = %+v", last)
	}
}

func TestClampPromptCacheKey(t *testing.T) {
	if got := ClampPromptCacheKey("short"); got != "short" {
		t.Fatalf("got %q", got)
	}
	if got := ClampPromptCacheKey(strings.Repeat("x", 70)); len(got) != 64 {
		t.Fatalf("len = %d", len(got))
	}
}
