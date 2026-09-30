package agent

import (
	"context"
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
	a.cfg.Before = func(_ context.Context, call llm.Block) (bool, string) {
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

// A message queued for follow-up delivery keeps the run alive when the
// loop would stop (pi's follow-up queue) instead of ending the run.
func TestSteerFollowUpContinuesRun(t *testing.T) {
	p := &scriptedProvider{script: []llm.Message{
		{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("first answer")}},
		{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("second answer")}},
	}}
	a := newTestAgent(p, &echoTool{})
	tr, _ := a.NewSession("t")

	// Queue the follow-up before the run even starts: the first stop
	// drains it and continues.
	a.FollowUp("and then do the second thing")
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

// Queue modes (pi's PendingMessageQueue): one-at-a-time delivers the
// oldest per drain, all delivers everything.
func TestQueueModes(t *testing.T) {
	a := New(Config{})
	a.Steer("one")
	a.Steer("two")
	a.Steer("three")
	if got := a.drain(&a.steer, QueueOneAtATime); len(got) != 1 || got[0] != "one" {
		t.Fatalf("one-at-a-time = %v", got)
	}
	if got := a.drain(&a.steer, QueueAll); len(got) != 2 || got[0] != "two" || got[1] != "three" {
		t.Fatalf("all = %v", got)
	}
	if got := a.drain(&a.steer, QueueAll); len(got) != 0 {
		t.Fatalf("drained = %v", got)
	}
}

// Steering queued while waiting is injected BEFORE the first turn (pi's
// runLoop start drain), so the first LLM call already sees it.
func TestSteerQueuedBeforeRunInjectedFirst(t *testing.T) {
	p := &scriptedProvider{script: []llm.Message{
		{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("ok")}},
	}}
	a := newTestAgent(p, &echoTool{})
	a.Steer("early correction")
	tr, _ := a.NewSession("t")
	if _, err := collect(t, a, tr, "go"); err != nil {
		t.Fatal(err)
	}
	msgs := tr.Messages()
	// system, user(go), user(early correction), assistant
	if len(msgs) != 4 || msgs[2].Content[0].Text != "early correction" {
		t.Fatalf("transcript = %+v", msgs)
	}
}
