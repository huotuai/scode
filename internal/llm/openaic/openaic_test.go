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

// pi's compat.supportsMidConvoSystemMessages: off (default) folds
// system deltas into the head; on, the head is the LEADING message
// alone and deltas ride in place — the head stays byte-stable across
// mid-session toggles.
func TestMidConvoSystemMessages(t *testing.T) {
	mk := func(t *testing.T) *llm.Transcript {
		tr, err := llm.NormalizeContext(llm.Context{
			SystemPrompt: "base prompt",
			Messages: []llm.Message{
				{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("u1")}, TS: 1},
				{Role: llm.RoleSystem, Sections: []llm.Section{{Name: "sandbox", Value: "mode: read-only"}}},
				{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("u2")}, TS: 2},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return tr
	}
	roles := func(req *wireRequest) []string {
		var out []string
		for _, m := range req.Messages {
			out = append(out, m.Role)
		}
		return out
	}

	// Default: collapsed — one system message whose text carries the delta.
	req, err := BuildRequest(model(), mk(t), llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(roles(req), ","); got != "system,user,user" {
		t.Fatalf("collapsed roles = %s", got)
	}
	head, _ := req.Messages[0].Content.(string)
	if !strings.Contains(head, "mode: read-only") {
		t.Fatalf("collapsed head lost the delta: %q", head)
	}

	// On: head is the leading prompt alone; the delta rides in place.
	m := model()
	m.Caps.MidConvoSystem = true
	req, err = BuildRequest(m, mk(t), llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(roles(req), ","); got != "system,user,system,user" {
		t.Fatalf("in-place roles = %s", got)
	}
	head, _ = req.Messages[0].Content.(string)
	if head != "base prompt" {
		t.Fatalf("head must be the leading prompt alone, got %q", head)
	}
	delta, _ := req.Messages[2].Content.(string)
	if !strings.Contains(delta, "<sandbox>") || !strings.Contains(delta, "mode: read-only") {
		t.Fatalf("in-place delta malformed: %q", delta)
	}
}

// pi parity: ALL historical thinking replays under the dialect key it
// arrived in (the block's Signature, recorded at stream time);
// signature-less thinking (legacy/foreign transcripts) never replays.
func TestReasoningReplayPiParity(t *testing.T) {
	mk := func(sig string) *llm.Transcript {
		tr, err := llm.NormalizeContext(llm.Context{
			SystemPrompt: "sys",
			Messages: []llm.Message{
				{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("q")}, TS: 1},
				{Role: llm.RoleAssistant, TS: 2, Content: []llm.Block{
					{Kind: llm.BlockThinking, Text: "deep thoughts", Signature: sig},
					llm.TextBlock("answer"),
				}},
				{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("q2")}, TS: 3},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return tr
	}

	// reasoning_content signature → reasoning_content field, any endpoint.
	req, err := BuildRequest(model(), mk("reasoning_content"), llm.StreamOptions{BaseURL: "https://api.deepseek.com/v1"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range req.Messages {
		if m.ReasoningContent != nil && *m.ReasoningContent == "deep thoughts" {
			found = true
		}
		if m.Reasoning != nil || m.ReasoningText != nil {
			t.Fatalf("wrong dialect key used: %+v", m)
		}
	}
	if !found {
		t.Fatal("reasoning_content-signed thinking not replayed")
	}

	// reasoning signature → reasoning field.
	req, err = BuildRequest(model(), mk("reasoning"), llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, m := range req.Messages {
		if m.Reasoning != nil && *m.Reasoning == "deep thoughts" {
			found = true
		}
	}
	if !found {
		t.Fatal("reasoning-signed thinking not replayed under reasoning")
	}

	// No signature (legacy/aborted) → no replay.
	req, err = BuildRequest(model(), mk(""), llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range req.Messages {
		if m.hasReasoning() {
			t.Fatalf("signature-less thinking replayed: %+v", m)
		}
	}
}

// pi's DeepSeek compat: a reasoning-capable model replays every
// assistant message with reasoning_content present — empty when the
// turn had no thinking (and the empty field does not rescue an
// otherwise empty message from being skipped).
func TestDeepSeekForcedReasoningContent(t *testing.T) {
	m := model()
	m.Reasoning = true
	req, err := BuildRequest(m, testTranscript(t), llm.StreamOptions{BaseURL: "https://api.deepseek.com/v1"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"reasoning_content":""`) {
		t.Fatalf("deepseek assistant messages must carry empty reasoning_content:\n%s", body)
	}

	// Non-reasoning model or non-deepseek endpoint: field absent.
	req, err = BuildRequest(model(), testTranscript(t), llm.StreamOptions{BaseURL: "https://api.deepseek.com/v1"})
	if err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(req)
	if strings.Contains(string(body), "reasoning_content") {
		t.Fatalf("non-reasoning model must not get the forced field:\n%s", body)
	}
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
	// Wire truth: tool calls carry "arguments" as a JSON string —
	// regression test for the parameters/arguments conflation that strict
	// endpoints reject with 422.
	if fn := asst.ToolCalls[0].Function; fn.Name != "bash" || fn.Arguments != `{"command":"ls"}` {
		t.Fatalf("function = %+v", fn)
	}
	wireJSON := mustJSON(req.Messages)
	if !strings.Contains(string(wireJSON), `"arguments":"{\"command\":\"ls\"}"`) {
		t.Fatalf("wire JSON lacks string arguments field: %s", wireJSON)
	}
	if strings.Contains(string(wireJSON), `"parameters"`) {
		t.Fatalf("tool-call replay must not use parameters: %s", wireJSON)
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

func TestBuildRequestToolCallEmptyArguments(t *testing.T) {
	tr, err := llm.NormalizeContext(llm.Context{
		SystemPrompt: "p",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("go")}, TS: 1},
			{Role: llm.RoleAssistant, TS: 2, Content: []llm.Block{llm.ToolCallBlock("c1", "ls")}}, // no arguments
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := BuildRequest(model(), tr, llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Messages[2].ToolCalls[0].Function.Arguments; got != "{}" {
		t.Fatalf("empty arguments = %q, want {}", got)
	}
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

func TestStreamEOFWithoutDoneWithFinishReason(t *testing.T) {
	// Some OpenAI-compatible relays close the connection without the [DONE]
	// sentinel. A finish_reason already proved generation completed, so this
	// must be a normal end, not a truncation error.
	const body = `data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}

data: {"choices":[{"index":0,"delta":{"content":"Hi"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(body)) //nolint:errcheck
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
	final := got[len(got)-1]
	if final.Type != llm.EventDone {
		t.Fatalf("terminal = %+v, want done", final)
	}
	if final.Message.StopReason != llm.StopEndTurn {
		t.Fatalf("stopReason = %s, want %s", final.Message.StopReason, llm.StopEndTurn)
	}
	if len(final.Message.Content) != 1 || final.Message.Content[0].Text != "Hi" {
		t.Fatalf("content = %+v", final.Message.Content)
	}
}

func TestStreamEOFWithoutDoneIsTruncation(t *testing.T) {
	// No [DONE] and no finish_reason: the relay cut the stream mid-generation.
	const body = `data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}

data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(body)) //nolint:errcheck
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
	if last.Type != llm.EventError || !strings.Contains(last.Message.Error, "stream ended before a terminal event") {
		t.Fatalf("last = %+v", last)
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

func TestBuildRequestThinkingWire(t *testing.T) {
	tr := testTranscript(t)

	// Effort-style endpoint: low|medium|high map to reasoning_effort.
	req, err := BuildRequest(model(), tr, llm.StreamOptions{ThinkingLevel: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if req.ReasoningEffort != "high" || req.Thinking != nil {
		t.Fatalf("effort = %q thinking = %+v", req.ReasoningEffort, req.Thinking)
	}
	// "off" on effort-style endpoints omits the field entirely.
	req, err = BuildRequest(model(), tr, llm.StreamOptions{ThinkingLevel: "off"})
	if err != nil {
		t.Fatal(err)
	}
	if req.ReasoningEffort != "" || req.Thinking != nil {
		t.Fatalf("off must omit both knobs: %q %+v", req.ReasoningEffort, req.Thinking)
	}

	// Zhipu-style endpoint: on/off switch, level-agnostic.
	zhipu := llm.StreamOptions{ThinkingLevel: "high", BaseURL: "https://open.bigmodel.cn/api/paas/v4"}
	req, err = BuildRequest(model(), tr, zhipu)
	if err != nil {
		t.Fatal(err)
	}
	if req.Thinking == nil || req.Thinking.Type != "enabled" || req.ReasoningEffort != "" {
		t.Fatalf("zhipu high = %+v effort=%q", req.Thinking, req.ReasoningEffort)
	}
	zhipu.ThinkingLevel = "off"
	req, err = BuildRequest(model(), tr, zhipu)
	if err != nil {
		t.Fatal(err)
	}
	if req.Thinking == nil || req.Thinking.Type != "disabled" {
		t.Fatalf("zhipu off = %+v", req.Thinking)
	}
}

// Recorded failure turns must replay as text — regression for the
// session-poisoning bug.
func TestBuildRequestErrorTurnSkipped(t *testing.T) {
	tr, err := llm.NormalizeContext(llm.Context{
		SystemPrompt: "p",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("go")}, TS: 1},
			{Role: llm.RoleAssistant, StopReason: llm.StopError, Error: "quota"},
			{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("again")}, TS: 2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := BuildRequest(model(), tr, llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// system + user + user: the error turn never reaches the wire.
	if len(req.Messages) != 3 {
		t.Fatalf("wire messages = %d, want 3", len(req.Messages))
	}
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			t.Fatalf("error turn leaked into the request: %+v", m)
		}
	}
	if req.Messages[2].Content != "again" {
		t.Fatalf("tail = %+v", req.Messages[2])
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

// Tool-result images follow in a user message only when the model
// accepts image input (pi's model.input gate); text-only models keep
// the placeholder text.
func TestBuildRequestToolResultImageGating(t *testing.T) {
	img := llm.Block{Kind: llm.BlockImage, MimeType: "image/png", Data: "aGk="}
	mk := func() *llm.Transcript {
		tr, err := llm.NormalizeContext(llm.Context{
			SystemPrompt: "p",
			Messages: []llm.Message{
				{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("look")}},
				{Role: llm.RoleAssistant, Content: []llm.Block{
					{Kind: llm.BlockToolCall, ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"x.png"}`)},
				}},
				{Role: llm.RoleTool, Content: []llm.Block{
					{Kind: llm.BlockToolResult, ID: "c1", Content: []llm.Block{img}},
				}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return tr
	}

	// Text-only model: no image message, placeholder in the tool result.
	req, err := BuildRequest(model(), mk(), llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range req.Messages {
		if m.Role == "user" {
			if parts, ok := m.Content.([]wireContent); ok {
				for _, p := range parts {
					if p.Type == "image_url" {
						t.Fatal("image forwarded to a text-only model")
					}
				}
			}
		}
		if m.Role == "tool" && m.Content != "(see attached image)" {
			t.Fatalf("tool placeholder = %v", m.Content)
		}
	}

	// Vision model: image rides a user message after the tool result.
	vm := model()
	vm.Caps.ImageInput = true
	req, err = BuildRequest(vm, mk(), llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, m := range req.Messages {
		if m.Role != "user" {
			continue
		}
		if parts, ok := m.Content.([]wireContent); ok {
			for _, p := range parts {
				if p.Type == "image_url" && p.ImageURL != nil && p.ImageURL.URL == "data:image/png;base64,aGk=" {
					saw = true
				}
			}
		}
	}
	if !saw {
		t.Fatal("image missing from the vision-model request")
	}
}

// Argument fragments that never form valid JSON are wrapped as a JSON
// string, never passed through as an invalid json.RawMessage.
func TestToJSONStringInvalidFragments(t *testing.T) {
	got := toJSONString(json.RawMessage(`{"command":oops`))
	if !json.Valid([]byte(got)) {
		t.Fatalf("toJSONString produced invalid JSON: %q", got)
	}
	var s string
	if err := json.Unmarshal([]byte(got), &s); err != nil || s != `{"command":oops` {
		t.Fatalf("wrapped = %q err=%v", got, err)
	}
	if got := toJSONString(json.RawMessage("{ \"a\" : 1 }")); got != `{"a":1}` {
		t.Fatalf("valid input should compact: %q", got)
	}
	if got := toJSONString(nil); got != `{}` {
		t.Fatalf("empty input should become {}: %q", got)
	}
}
