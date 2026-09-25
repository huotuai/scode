package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"scode/internal/llm"
)

// Steering queued mid-run (here: from the Before hook during the tool
// batch) lands in the transcript before the next LLM turn.
func TestSteerMidRun(t *testing.T) {
	p := &scriptedProvider{script: []llm.Message{
		{Role: llm.RoleAssistant, StopReason: llm.StopToolUse, Content: []llm.Block{
			{Kind: llm.BlockToolCall, ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"text":"a"}`)},
		}},
		{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("done")}},
	}}
	echo := &echoTool{}
	a := newTestAgent(p, echo)
	a.cfg.Before = func(call llm.Block) (bool, string) {
		a.Steer("correction: use option B")
		return false, ""
	}
	tr, _ := a.NewSession("t")
	_, runErr := collect(t, a, tr, "go")
	if runErr != nil {
		t.Fatal(runErr)
	}
	// The second provider call saw the steered user message after the
	// tool result.
	p.mu.Lock()
	defer p.mu.Unlock()
	second := p.streams[1].Messages()
	found := false
	for _, m := range second {
		if m.Role == llm.RoleUser && len(m.Content) > 0 && m.Content[0].Text == "correction: use option B" {
			found = true
		}
	}
	if !found {
		t.Fatal("steered message not injected before the next turn")
	}
}

// A message steered while the model wraps up keeps the run alive
// (follow-up semantics) instead of ending the run.
func TestSteerFollowUpContinuesRun(t *testing.T) {
	p := &scriptedProvider{script: []llm.Message{
		{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("first answer")}},
		{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("second answer")}},
	}}
	a := newTestAgent(p, &echoTool{})
	tr, _ := a.NewSession("t")

	// Queue the follow-up before the run even starts: the first stop
	// drains it and continues.
	a.Steer("and then do the second thing")
	events, runErr := collect(t, a, tr, "go")
	if runErr != nil {
		t.Fatal(runErr)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls != 2 {
		t.Fatalf("provider calls = %d, want 2 (follow-up continued the run)", p.calls)
	}
	var answers []string
	for _, ev := range events {
		if ev.Type == EvAssistant && ev.Message != nil {
			answers = append(answers, ev.Message.Content[0].Text)
		}
	}
	if strings.Join(answers, "|") != "first answer|second answer" {
		t.Fatalf("answers = %v", answers)
	}
}

// Steer with blank text is ignored; the queue drains clean.
func TestSteerBlankIgnored(t *testing.T) {
	p := &scriptedProvider{script: []llm.Message{
		{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("ok")}},
	}}
	a := newTestAgent(p, &echoTool{})
	tr, _ := a.NewSession("t")
	a.Steer("   ")
	_, runErr := collect(t, a, tr, "go")
	if runErr != nil {
		t.Fatal(runErr)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls != 1 {
		t.Fatalf("blank steer kept the run alive: calls = %d", p.calls)
	}
}
