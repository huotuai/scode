package anthropic

import (
	"encoding/json"
	"testing"

	"scode/internal/llm"
)

func buildTestTranscript(t *testing.T) *llm.Transcript {
	t.Helper()
	tr, err := llm.NormalizeContext(llm.Context{
		SystemPrompt: "You are scode.",
		Tools: []llm.Tool{
			{Name: "read", Description: "read a file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
			{Name: "bash", Description: "run a command"},
		},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("list files")}, TS: 1},
			{
				Role: llm.RoleAssistant, TS: 2, StopReason: llm.StopToolUse,
				Content: []llm.Block{llm.ToolCallBlock("t1", "bash")},
			},
			{
				Role: llm.RoleTool, TS: 3,
				Content: []llm.Block{{Kind: llm.BlockToolResult, ID: "t1", Content: []llm.Block{llm.TextBlock("a.txt\nb.txt")}}},
			},
			{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("thanks")}, TS: 4},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Give the tool call real arguments.
	msgs := tr.Messages()
	msgs[2].Content[0].Arguments = json.RawMessage(`{"command":"ls"}`)
	tr2, err := llm.NewTranscript(msgs[0], msgs[1:]...)
	if err != nil {
		t.Fatal(err)
	}
	return tr2
}

func modelCaps() llm.Model {
	return llm.Model{ID: "claude-x", Provider: "anthropic", APIShape: "anthropic-messages"}
}

func TestBuildRequestCachePlacement(t *testing.T) {
	tr := buildTestTranscript(t)
	req, err := BuildRequest(modelCaps(), tr, llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Breakpoint 1: last tool declaration only.
	if len(req.Tools) != 2 {
		t.Fatalf("tools = %d", len(req.Tools))
	}
	if req.Tools[0].CacheCtl != nil {
		t.Fatal("first tool must not carry cache_control")
	}
	if req.Tools[1].CacheCtl == nil || req.Tools[1].CacheCtl.Type != "ephemeral" {
		t.Fatalf("last tool cache_control = %+v", req.Tools[1].CacheCtl)
	}

	// Breakpoint 2: system block.
	if len(req.System) != 1 || req.System[0].CacheCtl == nil {
		t.Fatalf("system = %+v", req.System)
	}
	if req.System[0].Text != "You are scode." {
		t.Fatalf("system text = %q", req.System[0].Text)
	}

	// Breakpoint 3: tail block of the last user message.
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" {
		t.Fatalf("last message role = %s", last.Role)
	}
	tail := last.Content[len(last.Content)-1]
	if tail.CacheCtl == nil {
		t.Fatal("last user tail block lacks cache_control")
	}

	// Wire shape (pi): tool results group into their own user message,
	// consecutive same-role messages are NOT merged — 4 wire messages
	// (user, assistant tool_use, user tool_result, user thanks).
	if len(req.Messages) != 4 {
		t.Fatalf("messages = %d, want 4 (no same-role merging): %+v", len(req.Messages), req.Messages)
	}
	if req.Messages[1].Content[0].Type != "tool_use" || req.Messages[1].Content[0].ID != "t1" {
		t.Fatalf("assistant tool_use wrong: %+v", req.Messages[1].Content[0])
	}
	if req.Messages[2].Content[0].Type != "tool_result" || req.Messages[2].Content[0].ToolUseID != "t1" {
		t.Fatalf("tool_result folding wrong: %+v", req.Messages[2].Content[0])
	}
	if req.Messages[3].Content[0].Text != "thanks" {
		t.Fatalf("trailing user message missing: %+v", req.Messages[3].Content)
	}
}

func TestBuildRequestCacheNone(t *testing.T) {
	tr := buildTestTranscript(t)
	req, err := BuildRequest(modelCaps(), tr, llm.StreamOptions{Cache: llm.CacheNone})
	if err != nil {
		t.Fatal(err)
	}
	if req.Tools[len(req.Tools)-1].CacheCtl != nil || req.System[0].CacheCtl != nil {
		t.Fatal("cache none must not place breakpoints")
	}
	tail := req.Messages[len(req.Messages)-1].Content[0]
	if tail.CacheCtl != nil {
		t.Fatal("cache none must not mark the tail block")
	}
}

func TestBuildRequestLongRetention(t *testing.T) {
	tr := buildTestTranscript(t)
	m := modelCaps()
	m.Caps.LongCacheRetention = false
	req, err := BuildRequest(m, tr, llm.StreamOptions{Cache: llm.CacheLong})
	if err != nil {
		t.Fatal(err)
	}
	if req.Tools[len(req.Tools)-1].CacheCtl.TTL == "1h" {
		t.Fatal("1h TTL must require LongCacheRetention capability")
	}
	m2 := modelCaps()
	m2.Caps.LongCacheRetention = true
	req2, err := BuildRequest(m2, tr, llm.StreamOptions{Cache: llm.CacheLong})
	if err != nil {
		t.Fatal(err)
	}
	if got := req2.Tools[len(req2.Tools)-1].CacheCtl.TTL; got != "1h" {
		t.Fatalf("TTL = %q, want 1h", got)
	}
}

func TestBuildRequestThinkingAndTemperature(t *testing.T) {
	tr := buildTestTranscript(t)
	req, err := BuildRequest(modelCaps(), tr, llm.StreamOptions{ThinkingLevel: "low", Temperature: 0.7})
	if err != nil {
		t.Fatal(err)
	}
	if req.Thinking == nil || req.Thinking.BudgetTokens != 2048 {
		t.Fatalf("thinking = %+v", req.Thinking)
	}
	if req.Thinking.Type != "enabled" {
		t.Fatalf("thinking.type = %q — the API requires \"enabled\" or every thinking request 400s", req.Thinking.Type)
	}
	if req.Temperature != nil {
		t.Fatal("temperature must be dropped when thinking is on")
	}
	req2, err := BuildRequest(modelCaps(), tr, llm.StreamOptions{Temperature: 0.7})
	if err != nil {
		t.Fatal(err)
	}
	if req2.Temperature == nil || *req2.Temperature != 0.7 {
		t.Fatal("temperature must pass through when thinking is off")
	}
}

// A tool_use with no matching tool_result (interrupted turn) gets a
// synthetic error result IN PLACE (pi's transform-messages) — Anthropic
// 400s otherwise and the session bricks.
func TestBuildRequestOrphanToolUseSynthesized(t *testing.T) {
	tr, err := llm.NormalizeContext(llm.Context{
		SystemPrompt: "p",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("go")}, TS: 1},
			{Role: llm.RoleAssistant, StopReason: llm.StopToolUse, TS: 2, Content: []llm.Block{
				llm.ToolCallBlock("orphan_1", "bash"),
				llm.ToolCallBlock("answered_1", "read"),
			}},
			{Role: llm.RoleTool, TS: 3, Content: []llm.Block{
				{Kind: llm.BlockToolResult, ID: "answered_1", Content: []llm.Block{llm.TextBlock("ok")}},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := BuildRequest(modelCaps(), tr, llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Consecutive tool messages group into one user message: the real
	// result first, the synthetic orphan result right after it (in place,
	// not appended at the end of the conversation).
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" {
		t.Fatalf("last message role = %s", last.Role)
	}
	if len(last.Content) != 2 {
		t.Fatalf("grouped results = %+v", last.Content)
	}
	if last.Content[0].ToolUseID != "answered_1" || last.Content[0].IsError {
		t.Fatalf("real result = %+v", last.Content[0])
	}
	synth := last.Content[1]
	if synth.ToolUseID != "orphan_1" || !synth.IsError || synth.Content[0].Text != "No result provided" {
		t.Fatalf("synthesized result = %+v", synth)
	}
}

func TestBuildRequestSectionsRendering(t *testing.T) {
	tr, err := llm.NormalizeContext(llm.Context{
		SystemPrompt: "base",
		Messages:     []llm.Message{{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("hi")}, TS: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Patch a section mid-conversation.
	if err := tr.Append(llm.Message{Role: llm.RoleSystem, Sections: []llm.Section{{Name: "cwd", Value: "/home/v"}}}); err != nil {
		t.Fatal(err)
	}
	req, err := BuildRequest(modelCaps(), tr, llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := "base\n\n<cwd>\n/home/v\n</cwd>"
	if req.System[0].Text != want {
		t.Fatalf("system = %q, want %q", req.System[0].Text, want)
	}
}

// The cache contract end-to-end: growing the transcript must leave the
// serialized request prefix byte-identical — modulo cache_control markers,
// whose position legitimately moves to the new tail. Compare with markers
// stripped: content must be stable.
// Recorded failure turns must be SKIPPED in the request (pi's
// transform-messages): the model retries from the last valid state.
func TestBuildRequestErrorTurnSkipped(t *testing.T) {
	tr, err := llm.NormalizeContext(llm.Context{
		SystemPrompt: "p",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("go")}, TS: 1},
			{Role: llm.RoleAssistant, StopReason: llm.StopError, Error: "rate limited"},
			{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("again")}, TS: 2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := BuildRequest(modelCaps(), tr, llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("wire messages = %d, want 2 (user, user — error turn skipped)", len(req.Messages))
	}
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			t.Fatalf("error turn leaked into the request: %+v", m)
		}
	}
	if req.Messages[1].Content[0].Text != "again" {
		t.Fatalf("tail = %+v", req.Messages[1])
	}
}

func TestBuildRequestPrefixStability(t *testing.T) {
	tr := buildTestTranscript(t)
	req1, err := BuildRequest(modelCaps(), tr, llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sysBefore, _ := json.Marshal(req1.System)
	toolsBefore, _ := json.Marshal(req1.Tools)
	before := stripMarkers(req1.Messages)
	msgsBefore, _ := json.Marshal(before)

	if err := tr.Append(llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{llm.TextBlock("done")}, StopReason: llm.StopEndTurn, TS: 9}); err != nil {
		t.Fatal(err)
	}
	if err := tr.Append(llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("again")}, TS: 10}); err != nil {
		t.Fatal(err)
	}
	req2, err := BuildRequest(modelCaps(), tr, llm.StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sysAfter, _ := json.Marshal(req2.System)
	toolsAfter, _ := json.Marshal(req2.Tools)
	prior := stripMarkers(req2.Messages)

	if string(sysBefore) != string(sysAfter) {
		t.Fatal("system block changed across turns")
	}
	if string(toolsBefore) != string(toolsAfter) {
		t.Fatal("tools array changed across turns")
	}
	if len(prior) < len(before) {
		t.Fatal("request shrank")
	}
	prefix, _ := json.Marshal(prior[:len(before)])
	if string(msgsBefore) != string(prefix) {
		t.Fatalf("prior messages changed across turns:\nbefore: %s\nprefix:  %s", msgsBefore, prefix)
	}
}

// stripMarkers deep-copies wire messages with cache_control removed.
func stripMarkers(msgs []wireMessage) []wireMessage {
	out := make([]wireMessage, len(msgs))
	for i, m := range msgs {
		out[i].Role = m.Role
		out[i].Content = make([]wireBlock, len(m.Content))
		for j, b := range m.Content {
			b.CacheCtl = nil
			out[i].Content[j] = b
		}
	}
	return out
}

// Regression: thinking medium/high failed request building with the
// default max_tokens (budget >= 8192). pi's adjustMaxTokensForThinking:
// a caller cap stacks the budget on top; without one, thinking fits
// inside the model cap, always leaving minAnswerTokens of answer room.
func TestBuildRequestThinkingBudgetStacksOnMaxTokens(t *testing.T) {
	tr := buildTestTranscript(t)

	// No caller cap, no model cap: thinking fits inside the flat default.
	req, err := BuildRequest(modelCaps(), tr, llm.StreamOptions{ThinkingLevel: "medium"})
	if err != nil {
		t.Fatal(err)
	}
	if req.MaxTokens != DefaultMaxOut {
		t.Fatalf("medium max_tokens = %d, want %d", req.MaxTokens, DefaultMaxOut)
	}
	if req.Thinking == nil || req.Thinking.BudgetTokens != DefaultMaxOut-minAnswerTokens {
		t.Fatalf("medium budget = %+v, want %d", req.Thinking, DefaultMaxOut-minAnswerTokens)
	}

	// Caller cap: the budget rides on top (thinking AND answer must fit).
	req, err = BuildRequest(modelCaps(), tr, llm.StreamOptions{ThinkingLevel: "high", MaxTokens: 8192})
	if err != nil {
		t.Fatal(err)
	}
	if req.Thinking.BudgetTokens != 16384 || req.MaxTokens != 8192+16384 {
		t.Fatalf("high with caller cap: budget=%d max=%d", req.Thinking.BudgetTokens, req.MaxTokens)
	}

	// Model cap without caller cap: max_tokens is the model cap and the
	// budget shrinks to leave answer room.
	m := modelCaps()
	m.MaxTokens = 10000
	req, err = BuildRequest(m, tr, llm.StreamOptions{ThinkingLevel: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if req.MaxTokens != 10000 {
		t.Fatalf("model-capped max_tokens = %d, want 10000", req.MaxTokens)
	}
	if req.Thinking.BudgetTokens != 10000-minAnswerTokens {
		t.Fatalf("clamped budget = %d, want %d", req.Thinking.BudgetTokens, 10000-minAnswerTokens)
	}

	// Caller cap stacked with the budget still bends to the model cap.
	req, err = BuildRequest(m, tr, llm.StreamOptions{ThinkingLevel: "high", MaxTokens: 8192})
	if err != nil {
		t.Fatal(err)
	}
	if req.MaxTokens != 10000 || req.Thinking.BudgetTokens != 10000-minAnswerTokens {
		t.Fatalf("caller+model cap: budget=%d max=%d", req.Thinking.BudgetTokens, req.MaxTokens)
	}
}
