package google

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
				Model: "gemini-x", Provider: "google",
				Content: []llm.Block{
					{Kind: llm.BlockToolCall, ID: "call_1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)},
				},
			},
			{
				Role: llm.RoleTool, TS: 3,
				Content: []llm.Block{
					{Kind: llm.BlockToolResult, ID: "call_1", Name: "bash", Content: []llm.Block{llm.TextBlock("a.txt")}},
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
	return llm.Model{ID: "gemini-x", Provider: "google", APIShape: "google-generative-ai"}
}

func TestBuildRequestShape(t *testing.T) {
	req := BuildRequest(model(), testTranscript(t), llm.StreamOptions{})
	if req.SystemInstruction == nil || req.SystemInstruction.Parts[0].Text != "You are scode." {
		t.Fatalf("systemInstruction = %+v", req.SystemInstruction)
	}
	if len(req.Tools) != 1 || req.Tools[0].FunctionDeclarations[0].Name != "bash" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if string(req.Tools[0].FunctionDeclarations[0].ParametersJSONSchema) == "" {
		t.Fatal("parametersJsonSchema must be set")
	}
	// Contents: user, model(functionCall), user(functionResponse merged).
	if len(req.Contents) != 3 {
		b, _ := json.Marshal(req.Contents)
		t.Fatalf("contents = %s", b)
	}
	if req.Contents[0].Role != "user" || req.Contents[1].Role != "model" {
		t.Fatalf("roles = %+v", req.Contents)
	}
	fc := req.Contents[1].Parts[0].FunctionCall
	if fc == nil || fc.Name != "bash" || string(fc.Args) != `{"command":"ls"}` {
		t.Fatalf("functionCall = %+v", req.Contents[1].Parts[0])
	}
	if fc.ID != "" {
		t.Fatalf("gemini-x is not an id-requiring model; id = %q", fc.ID)
	}
	fr := req.Contents[2].Parts[0].FunctionResponse
	if fr == nil || fr.Name != "bash" {
		t.Fatalf("functionResponse = %+v", req.Contents[2].Parts[0])
	}
	if string(fr.Response) != `{"output":"a.txt"}` {
		t.Fatalf("response body = %s", fr.Response)
	}
}

func TestBuildRequestToolCallIDGate(t *testing.T) {
	g3 := model()
	g3.ID = "gemini-3-pro-preview"
	req := BuildRequest(g3, testTranscript(t), llm.StreamOptions{})
	fc := req.Contents[1].Parts[0].FunctionCall
	fr := req.Contents[2].Parts[0].FunctionResponse
	if fc.ID != "call_1" || fr.ID != "call_1" {
		t.Fatalf("gemini-3 requires ids: call=%q response=%q", fc.ID, fr.ID)
	}
}

func TestBuildRequestThinkingConfig(t *testing.T) {
	// Level model (gemini-3): thinkingLevel control.
	m := model()
	m.ID = "gemini-3-pro-preview"
	m.Reasoning = true
	req := BuildRequest(m, testTranscript(t), llm.StreamOptions{ThinkingLevel: "high"})
	tc := req.GenerationConfig.ThinkingConfig
	if tc == nil || tc.ThinkingLevel != "HIGH" || !tc.IncludeThoughts {
		t.Fatalf("thinkingConfig = %+v", tc)
	}
	// Budget model (2.5-pro): thinkingBudget table.
	m.ID = "gemini-2.5-pro"
	req = BuildRequest(m, testTranscript(t), llm.StreamOptions{ThinkingLevel: "low"})
	tc = req.GenerationConfig.ThinkingConfig
	if tc == nil || tc.ThinkingBudget == nil || *tc.ThinkingBudget != 2048 {
		t.Fatalf("budget config = %+v", tc)
	}
	// Off on a reasoning model: budget 0 (pi's disabled config).
	req = BuildRequest(m, testTranscript(t), llm.StreamOptions{ThinkingLevel: "off"})
	tc = req.GenerationConfig.ThinkingConfig
	if tc == nil || tc.ThinkingBudget == nil || *tc.ThinkingBudget != 0 || tc.IncludeThoughts {
		t.Fatalf("disabled config = %+v", tc)
	}
	// Non-reasoning model: never a thinkingConfig.
	nr := model()
	req = BuildRequest(nr, testTranscript(t), llm.StreamOptions{ThinkingLevel: "high"})
	if req.GenerationConfig != nil && req.GenerationConfig.ThinkingConfig != nil {
		t.Fatalf("non-reasoning thinkingConfig = %+v", req.GenerationConfig.ThinkingConfig)
	}
}

func TestBuildRequestMaxTokens(t *testing.T) {
	req := BuildRequest(model(), testTranscript(t), llm.StreamOptions{MaxTokens: 1234})
	if req.GenerationConfig == nil || req.GenerationConfig.MaxOutputTokens != 1234 {
		t.Fatalf("generationConfig = %+v", req.GenerationConfig)
	}
}

func TestCrossProviderThinkingLowers(t *testing.T) {
	// Anthropic thinking replayed to Gemini becomes a plain text part.
	tr, err := llm.NormalizeContext(llm.Context{
		SystemPrompt: "p",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("hi")}, TS: 1},
			{
				Role: llm.RoleAssistant, TS: 2, StopReason: llm.StopEndTurn,
				Model: "claude-x", Provider: "anthropic",
				Content: []llm.Block{
					{Kind: llm.BlockThinking, Text: "pondering", Signature: "AAA=="},
					{Kind: llm.BlockText, Text: "answer"},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := BuildRequest(model(), tr, llm.StreamOptions{})
	parts := req.Contents[1].Parts
	if len(parts) != 2 || parts[0].Thought || parts[0].Text != "pondering" || parts[0].ThoughtSignature != "" {
		t.Fatalf("parts = %+v", parts)
	}
}

func TestSameModelThoughtSignatureReplay(t *testing.T) {
	// Same-model thinking keeps thought: true and a valid base64
	// signature; invalid base64 is dropped (pi's resolveThoughtSignature).
	sig := "QUJDREVGRw==" // valid base64, len%4==0
	tr, err := llm.NormalizeContext(llm.Context{
		SystemPrompt: "p",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("hi")}, TS: 1},
			{
				Role: llm.RoleAssistant, TS: 2, StopReason: llm.StopEndTurn,
				Model: "gemini-x", Provider: "google",
				Content: []llm.Block{
					{Kind: llm.BlockThinking, Text: "hmm", Signature: sig},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := BuildRequest(model(), tr, llm.StreamOptions{})
	p := req.Contents[1].Parts[0]
	if !p.Thought || p.Text != "hmm" || p.ThoughtSignature != sig {
		t.Fatalf("part = %+v", p)
	}
}

func runStream(t *testing.T, p *Provider, body string, m llm.Model) []llm.Event {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("alt") != "sse" {
			t.Errorf("alt = %s", r.URL.Query())
		}
		if r.Header.Get("x-goog-api-key") != "k" {
			t.Errorf("api key header = %q", r.Header.Get("x-goog-api-key"))
		}
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	ch, err := p.Stream(context.Background(), m, testTranscript(t), llm.StreamOptions{APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	var events []llm.Event
	for e := range ch {
		events = append(events, e)
	}
	return events
}

func sseChunks(payloads ...string) string {
	var sb strings.Builder
	for _, p := range payloads {
		fmt.Fprintf(&sb, "data: %s\n\n", p)
	}
	return sb.String()
}

func TestStreamTextAndUsage(t *testing.T) {
	body := sseChunks(
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":" world"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"cachedContentTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":2,"totalTokenCount":19}}`,
	)
	events := runStream(t, New("k", ""), body, model())
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
	if msg.Usage.Input != 2 || msg.Usage.CacheRead != 10 || msg.Usage.Output != 7 {
		t.Fatalf("usage = %+v", msg.Usage)
	}
}

func TestStreamThinkingAndSignature(t *testing.T) {
	body := sseChunks(
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"thinking","thought":true,"thoughtSignature":"QUJD"}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"answer","thoughtSignature":"REVG"}]},"finishReason":"STOP"}]}`,
	)
	events := runStream(t, New("k", ""), body, model())
	last := events[len(events)-1]
	msg := last.Message
	if len(msg.Content) != 2 {
		t.Fatalf("content = %+v", msg.Content)
	}
	if msg.Content[0].Kind != llm.BlockThinking || msg.Content[0].Signature != "QUJD" {
		t.Fatalf("thinking = %+v", msg.Content[0])
	}
	if msg.Content[1].Kind != llm.BlockText || msg.Content[1].Signature != "REVG" {
		t.Fatalf("text = %+v", msg.Content[1])
	}
}

func TestStreamFunctionCall(t *testing.T) {
	body := sseChunks(
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"bash","args":{"command":"ls"}}}]},"finishReason":"STOP"}]}`,
	)
	events := runStream(t, New("k", ""), body, model())
	if err := llm.ValidateStream(events); err != nil {
		t.Fatalf("stream contract: %v", err)
	}
	last := events[len(events)-1]
	if last.Reason != llm.StopToolUse {
		t.Fatalf("reason = %q", last.Reason)
	}
	call := last.Message.Content[0]
	if call.Kind != llm.BlockToolCall || call.Name != "bash" || string(call.Arguments) != `{"command":"ls"}` {
		t.Fatalf("call = %+v", call)
	}
	if !strings.HasPrefix(call.ID, "bash_") {
		t.Fatalf("fallback id = %q", call.ID)
	}
}

func TestStreamFinishError(t *testing.T) {
	body := sseChunks(
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"SAFETY"}]}`,
	)
	events := runStream(t, New("k", ""), body, model())
	last := events[len(events)-1]
	if last.Type != llm.EventError || last.Reason != llm.StopError {
		t.Fatalf("terminal = %+v", last)
	}
	if !strings.Contains(last.Message.Error, "SAFETY") {
		t.Fatalf("error = %q", last.Message.Error)
	}
}

func TestStreamPrematureEnd(t *testing.T) {
	body := sseChunks(`{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]}}]}`)
	events := runStream(t, New("k", ""), body, model())
	last := events[len(events)-1]
	if last.Type != llm.EventError || !strings.Contains(last.Message.Error, "finish reason") {
		t.Fatalf("terminal = %+v", last)
	}
}

func TestStreamHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"bad key"}}`)
	}))
	t.Cleanup(srv.Close)
	p := New("k", srv.URL)
	ch, err := p.Stream(context.Background(), model(), testTranscript(t), llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var last llm.Event
	for e := range ch {
		last = e
	}
	if last.Type != llm.EventError || !strings.Contains(last.Message.Error, "PERMISSION_DENIED: bad key") {
		t.Fatalf("error event = %+v", last)
	}
}
