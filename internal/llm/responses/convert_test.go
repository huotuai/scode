package responses

import (
	"encoding/json"
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
				Model: "gpt-x", Provider: "openai-responses",
				Content: []llm.Block{
					{Kind: llm.BlockText, Text: "checking", Signature: `{"v":1,"id":"msg_abc"}`},
					{Kind: llm.BlockToolCall, ID: "call_1|fc_1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)},
				},
			},
			{
				Role: llm.RoleTool, TS: 3,
				Content: []llm.Block{
					{Kind: llm.BlockToolResult, ID: "call_1|fc_1", Name: "bash", Content: []llm.Block{llm.TextBlock("a.txt")}},
				},
			},
			{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("thanks")}, TS: 4},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func model() llm.Model {
	return llm.Model{ID: "gpt-x", Provider: "openai-responses", APIShape: "openai-responses"}
}

// MidConvoSystem on: the head is the leading system item alone and a
// later system delta rides in place; off (default) it collapses into
// the head item.
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

	// Default: one system item, head carries the delta.
	params := buildOpenAIParams(model(), mk(t), llm.StreamOptions{})
	if len(params.Input) != 3 {
		b, _ := json.Marshal(params.Input)
		t.Fatalf("collapsed input = %d items, want 3: %s", len(params.Input), b)
	}
	if sys := params.Input[0].(systemItem); !strings.Contains(sys.Content, "mode: read-only") {
		t.Fatalf("collapsed head lost the delta: %q", sys.Content)
	}

	// On: system,user,system,user — head untouched by the delta.
	m := model()
	m.Caps.MidConvoSystem = true
	params = buildOpenAIParams(m, mk(t), llm.StreamOptions{})
	if len(params.Input) != 4 {
		b, _ := json.Marshal(params.Input)
		t.Fatalf("in-place input = %d items, want 4: %s", len(params.Input), b)
	}
	if sys := params.Input[0].(systemItem); sys.Content != "base prompt" {
		t.Fatalf("head must be the leading prompt alone: %q", sys.Content)
	}
	delta, ok := params.Input[2].(systemItem)
	if !ok || !strings.Contains(delta.Content, "mode: read-only") {
		t.Fatalf("in-place delta missing: %+v", params.Input[2])
	}
}

func TestBuildParamsShape(t *testing.T) {
	params := buildOpenAIParams(model(), testTranscript(t), llm.StreamOptions{})
	if !params.Stream || params.Store {
		t.Fatal("stream must be true and store false (stateless replay)")
	}
	// Input: system, user, message item, function_call,
	// function_call_output, user.
	if len(params.Input) != 6 {
		b, _ := json.Marshal(params.Input)
		t.Fatalf("input items = %d, want 6: %s", len(params.Input), b)
	}
	sys, ok := params.Input[0].(systemItem)
	if !ok || sys.Role != "system" || sys.Content != "You are scode." {
		t.Fatalf("system item = %+v", params.Input[0])
	}
	if u, ok := params.Input[1].(userItem); !ok || u.Role != "user" {
		t.Fatalf("input[1] = %+v", params.Input[1])
	}
	msg, ok := params.Input[2].(messageItem)
	if !ok || msg.ID != "msg_abc" || msg.Status != "completed" || msg.Role != "assistant" {
		t.Fatalf("message item = %+v", params.Input[2])
	}
	if msg.Content[0].Text != "checking" {
		t.Fatalf("message text = %q", msg.Content[0].Text)
	}
	fc, ok := params.Input[3].(functionCallItem)
	if !ok || fc.CallID != "call_1" || fc.ID != "fc_1" || fc.Name != "bash" {
		t.Fatalf("function_call = %+v", params.Input[3])
	}
	// Wire truth: arguments is a JSON-encoded STRING.
	if fc.Arguments != `{"command":"ls"}` {
		t.Fatalf("arguments = %q", fc.Arguments)
	}
	fco, ok := params.Input[4].(functionCallOutputItem)
	if !ok || fco.CallID != "call_1" || fco.Output != "a.txt" {
		t.Fatalf("function_call_output = %+v", params.Input[4])
	}
}

func TestBuildParamsSystemRole(t *testing.T) {
	tr := testTranscript(t)
	// Non-reasoning model: "system" role; reasoning model: "developer".
	params := buildOpenAIParams(model(), tr, llm.StreamOptions{})
	sys, ok := params.Input[0].(systemItem)
	if !ok || sys.Role != "system" {
		t.Fatalf("non-reasoning system item = %+v", params.Input[0])
	}

	tr2, err := llm.NormalizeContext(llm.Context{
		SystemPrompt: "prompt here",
		Messages:     []llm.Message{{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("hi")}, TS: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	params = buildOpenAIParams(model(), tr2, llm.StreamOptions{})
	sys, ok = params.Input[0].(systemItem)
	if !ok || sys.Role != "system" || sys.Content != "prompt here" {
		t.Fatalf("system item = %+v", params.Input[0])
	}

	rm := model()
	rm.Reasoning = true
	params = buildOpenAIParams(rm, tr2, llm.StreamOptions{})
	sys, ok = params.Input[0].(systemItem)
	if !ok || sys.Role != "developer" {
		t.Fatalf("reasoning model system item = %+v", params.Input[0])
	}
}

func TestBuildParamsReasoning(t *testing.T) {
	tr := testTranscript(t)
	m := model()
	m.Reasoning = true

	// Explicit level: effort + summary auto + encrypted include.
	p := buildOpenAIParams(m, tr, llm.StreamOptions{ThinkingLevel: "high"})
	if p.Reasoning == nil || p.Reasoning.Effort != "high" || p.Reasoning.Summary != "auto" {
		t.Fatalf("reasoning = %+v", p.Reasoning)
	}
	if len(p.Include) != 1 || p.Include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %v", p.Include)
	}
	// No level on a reasoning model: explicit off (pi's default path).
	p = buildOpenAIParams(m, tr, llm.StreamOptions{})
	if p.Reasoning == nil || p.Reasoning.Effort != "none" {
		t.Fatalf("reasoning (no level) = %+v", p.Reasoning)
	}
	if len(p.Include) != 0 {
		t.Fatalf("include must be empty on the none path: %v", p.Include)
	}
	// Non-reasoning model: never a reasoning parameter.
	p = buildOpenAIParams(model(), tr, llm.StreamOptions{ThinkingLevel: "high"})
	if p.Reasoning != nil {
		t.Fatalf("non-reasoning model got reasoning param: %+v", p.Reasoning)
	}
}

func TestBuildParamsCache(t *testing.T) {
	tr := testTranscript(t)
	long := strings.Repeat("s", 100)
	p := buildOpenAIParams(model(), tr, llm.StreamOptions{PromptCacheKey: long})
	if p.PromptCacheKey != strings.Repeat("s", 64) {
		t.Fatalf("prompt_cache_key not clamped to 64: %q", p.PromptCacheKey)
	}
	if p.PromptCacheRetention != "" {
		t.Fatalf("short retention must not set retention: %q", p.PromptCacheRetention)
	}
	p = buildOpenAIParams(model(), tr, llm.StreamOptions{PromptCacheKey: "sess", Cache: llm.CacheLong})
	if p.PromptCacheRetention != "24h" {
		t.Fatalf("long retention = %q, want 24h", p.PromptCacheRetention)
	}
	p = buildOpenAIParams(model(), tr, llm.StreamOptions{PromptCacheKey: "sess", Cache: llm.CacheNone})
	if p.PromptCacheKey != "" || p.PromptCacheRetention != "" {
		t.Fatalf("cache none must drop key and retention: %+v", p)
	}
}

func TestBuildParamsMaxOutputTokensFloor(t *testing.T) {
	p := buildOpenAIParams(model(), testTranscript(t), llm.StreamOptions{MaxTokens: 4})
	if p.MaxOutputTokens != minOutputTokens {
		t.Fatalf("max_output_tokens = %d, want floor %d", p.MaxOutputTokens, minOutputTokens)
	}
}

func TestConvertToolsStrict(t *testing.T) {
	tools := []llm.Tool{{Name: "t", Parameters: json.RawMessage(`{"type":"object"}`)}}
	// OpenAI default: no strict field.
	out := convertTools(tools, false)
	if out[0].Strict != nil {
		t.Fatalf("openai tools must omit strict: %+v", out[0])
	}
	// Azure default (supportsStrictMode true): strict: false.
	out = convertTools(tools, true)
	if out[0].Strict == nil || *out[0].Strict {
		t.Fatalf("azure tools must carry strict:false: %+v", out[0])
	}
	b, _ := json.Marshal(out[0])
	if !strings.Contains(string(b), `"strict":false`) {
		t.Fatalf("wire JSON lacks strict:false: %s", b)
	}
}

func TestNormalizeToolCallID(t *testing.T) {
	src := llm.Message{Role: llm.RoleAssistant, Provider: "anthropic", Model: "claude-x"}
	// Anthropic-style id replayed to openai-responses: chars are legal,
	// no composite split.
	if got := normalizeToolCallID("toolu_01ABC", "openai-responses", openAIToolCallProviders, src); got != "toolu_01ABC" {
		t.Fatalf("plain id = %q", got)
	}
	// Composite foreign id: item part hashed into the fc_ space.
	got := normalizeToolCallID("call_1|rs_long_item", "openai-responses", openAIToolCallProviders, src)
	callID, itemID, _ := strings.Cut(got, "|")
	if callID != "call_1" || !strings.HasPrefix(itemID, "fc_") || len(got) > 64+64+1 {
		t.Fatalf("composite = %q", got)
	}
	// Disallowed target provider: whole id flattened.
	if got := normalizeToolCallID("call_1|fc_1", "bedrock", openAIToolCallProviders, src); strings.Contains(got, "|") {
		t.Fatalf("disallowed provider id = %q", got)
	}
	// Long ids truncate to 64 per part with trailing underscores stripped.
	long := strings.Repeat("x", 80) + "|||" // '|' splits: callID part cleans to x's
	got = normalizeToolCallID(long, "openai-responses", openAIToolCallProviders, src)
	if c, _, _ := strings.Cut(got, "|"); len(c) > 64 {
		t.Fatalf("call part not truncated: %q", c)
	}
}

func TestConvertAssistantCrossProviderThinking(t *testing.T) {
	// Anthropic thinking replayed to the Responses API lowers to plain
	// assistant text (pi's transformMessages cross-model rule).
	tr, err := llm.NormalizeContext(llm.Context{
		SystemPrompt: "p",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("hi")}, TS: 1},
			{
				Role: llm.RoleAssistant, TS: 2, StopReason: llm.StopEndTurn,
				Model: "claude-x", Provider: "anthropic",
				Content: []llm.Block{
					{Kind: llm.BlockThinking, Text: "pondering", Signature: "sig-from-anthropic"},
					{Kind: llm.BlockText, Text: "answer"},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	params := buildOpenAIParams(model(), tr, llm.StreamOptions{})
	// Expect: system, user, message("pondering"), message("answer").
	if len(params.Input) != 4 {
		b, _ := json.Marshal(params.Input)
		t.Fatalf("input = %s", b)
	}
	m1, ok := params.Input[2].(messageItem)
	if !ok || m1.Content[0].Text != "pondering" {
		t.Fatalf("cross-provider thinking = %+v", params.Input[2])
	}
	if m1.ID != "msg_pi_1" { // first text block of message index 1
		t.Fatalf("fallback msg id = %q", m1.ID)
	}
}

func TestConvertAssistantSameModelReasoningReplay(t *testing.T) {
	// A stored reasoning item replays verbatim from the thinking
	// signature.
	itemJSON := `{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"s"}],"encrypted_content":"enc"}`
	tr, err := llm.NormalizeContext(llm.Context{
		SystemPrompt: "p",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("hi")}, TS: 1},
			{
				Role: llm.RoleAssistant, TS: 2, StopReason: llm.StopEndTurn,
				Model: "gpt-x", Provider: "openai-responses",
				Content: []llm.Block{
					{Kind: llm.BlockThinking, Text: "s", Signature: itemJSON},
					{Kind: llm.BlockText, Text: "answer", Signature: `{"v":1,"id":"msg_1","phase":"final_answer"}`},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	params := buildOpenAIParams(model(), tr, llm.StreamOptions{})
	if len(params.Input) != 4 {
		b, _ := json.Marshal(params.Input)
		t.Fatalf("input = %s", b)
	}
	ri, ok := params.Input[2].(reasoningItem)
	if !ok || string(ri) != itemJSON {
		b, _ := json.Marshal(params.Input[2])
		t.Fatalf("reasoning replay = %s", b)
	}
	// The named type must marshal as RAW JSON on the wire: it does not
	// inherit json.RawMessage.MarshalJSON, and without the override
	// encoding/json treats it as a plain byte slice and emits a base64
	// string — every stateless-replay turn would 400.
	if b, err := json.Marshal(params.Input[2]); err != nil || string(b) != itemJSON {
		t.Fatalf("reasoning item marshals as %s (err=%v), want verbatim %s", b, err, itemJSON)
	}
	msg := params.Input[3].(messageItem)
	if msg.ID != "msg_1" || msg.Phase != "final_answer" {
		t.Fatalf("message item = %+v", msg)
	}
}

func TestConvertToolResultImages(t *testing.T) {
	block := llm.Block{
		Kind: llm.BlockToolResult, ID: "c1", Name: "shot",
		Content: []llm.Block{{Kind: llm.BlockImage, MimeType: "image/png", Data: "AAAA"}},
	}
	// Vision model: structured output with input_image.
	vision := model()
	vision.Caps.ImageInput = true
	out := convertToolResultOutput(vision, block)
	arr, ok := out.([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("vision output = %+v", out)
	}
	img, ok := arr[0].(inputImage)
	if !ok || img.ImageURL != "data:image/png;base64,AAAA" || img.Detail != "auto" {
		t.Fatalf("image part = %+v", arr[0])
	}
	// Text-only model: the pi downgrade placeholder.
	out = convertToolResultOutput(model(), block)
	if out != "(tool image omitted: model does not support images)" {
		t.Fatalf("non-vision output = %v", out)
	}
}

func TestParseTextSignature(t *testing.T) {
	id, phase, ok := parseTextSignature(`{"v":1,"id":"msg_x","phase":"commentary"}`)
	if !ok || id != "msg_x" || phase != "commentary" {
		t.Fatalf("v1 parse = %q %q %v", id, phase, ok)
	}
	id, phase, ok = parseTextSignature("msg_plain")
	if !ok || id != "msg_plain" || phase != "" {
		t.Fatalf("legacy parse = %q %q %v", id, phase, ok)
	}
	if _, _, ok := parseTextSignature(""); ok {
		t.Fatal("empty signature must not parse")
	}
}

func TestAzureParams(t *testing.T) {
	tr := testTranscript(t)
	params := buildAzureParams(model(), tr, llm.StreamOptions{PromptCacheKey: "sess", Cache: llm.CacheNone})
	// Azure sends the cache key regardless of retention (pi).
	if params.PromptCacheKey != "sess" {
		t.Fatalf("azure prompt_cache_key = %q", params.PromptCacheKey)
	}
	if params.PromptCacheRetention != "" {
		t.Fatalf("azure must not send retention: %q", params.PromptCacheRetention)
	}
	if len(params.Tools) != 1 || params.Tools[0].Strict == nil {
		t.Fatalf("azure tools must carry strict: %+v", params.Tools)
	}
}

func TestNormalizeAzureBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://my-resource.openai.azure.com":                     "https://my-resource.openai.azure.com/openai/v1",
		"https://x.cognitiveservices.azure.com/openai":             "https://x.cognitiveservices.azure.com/openai/v1",
		"https://x.ai.azure.com/openai/v1/responses":               "https://x.ai.azure.com/openai/v1",
		"https://x.openai.azure.com/openai?api-version=2024-12-01": "https://x.openai.azure.com/openai/v1",
		"https://my-proxy.example.com/v1":                          "https://my-proxy.example.com/v1",
		"https://my-proxy.example.com/v1?custom=true":              "https://my-proxy.example.com/v1?custom=true",
	}
	for in, want := range cases {
		got, err := normalizeAzureBaseURL(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != want {
			t.Fatalf("normalizeAzureBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := normalizeAzureBaseURL("not-a-url"); err == nil {
		t.Fatal("invalid URL must error")
	}
}
