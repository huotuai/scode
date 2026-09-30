package agent_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"scode/internal/agent"
	"scode/internal/llm"
	"scode/internal/tools"
)

// scripted drives the loop with canned assistant messages.
type scripted struct {
	mu     sync.Mutex
	calls  int
	script []llm.Message
	last   *llm.Transcript
}

func (p *scripted) Name() string { return "scripted" }
func (p *scripted) Caps() llm.Capabilities {
	return llm.Capabilities{}
}

func (p *scripted) Stream(_ context.Context, _ llm.Model, t *llm.Transcript, _ llm.StreamOptions) (<-chan llm.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := &llm.Transcript{}
	for _, m := range t.Messages() {
		_ = cp.Append(m)
	}
	p.last = cp
	i := p.calls
	p.calls++
	if i >= len(p.script) {
		return nil, os.ErrDeadlineExceeded
	}
	msg := p.script[i]
	out := make(chan llm.Event, 8)
	go func() {
		defer close(out)
		m := msg
		out <- llm.Event{Type: llm.EventStart, Message: &m}
		f := msg
		out <- llm.Event{Type: llm.EventDone, Message: &f, Reason: msg.StopReason}
	}()
	return out, nil
}

// The M3 acceptance in miniature: the model reads a file, fixes a bug via
// edit, verifies with bash, and reports — real tools, scripted model.
func TestEndToEndFixBug(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "calc.py"), []byte("def add(a, b):\n    return a - b\n"), 0o644) //nolint:errcheck

	p := &scripted{script: []llm.Message{
		{Role: llm.RoleAssistant, StopReason: llm.StopToolUse, Content: []llm.Block{
			{Kind: llm.BlockToolCall, ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"calc.py"}`)},
		}},
		{Role: llm.RoleAssistant, StopReason: llm.StopToolUse, Content: []llm.Block{
			{Kind: llm.BlockToolCall, ID: "c2", Name: "edit", Arguments: json.RawMessage(`{"path":"calc.py","edits":[{"oldText":"return a - b","newText":"return a + b"}]}`)},
		}},
		{Role: llm.RoleAssistant, StopReason: llm.StopToolUse, Content: []llm.Block{
			{Kind: llm.BlockToolCall, ID: "c3", Name: "bash", Arguments: json.RawMessage(`{"command":"python -c \"import calc; assert calc.add(1,2)==3; print('OK')\""}`)},
		}},
		{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("Fixed: add() now returns a + b, verified.")}},
	}}

	a := agent.New(agent.Config{
		Provider: p,
		Model:    llm.Model{ID: "test", Provider: "scripted"},
		Tools:    tools.NewCodingRegistry(),
		CWD:      dir,
		// Unfenced on purpose: this test exercises the edit path, not the
		// sandbox, and an absent policy is fail-closed by contract.
		Sandbox: func() *agent.SandboxPolicy { return agent.UnfencedPolicy(dir) },
	})
	tr, err := a.NewSession("You fix bugs.")
	if err != nil {
		t.Fatal(err)
	}

	out := make(chan agent.Event, 256)
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	go func() { done <- a.Prompt(ctx, tr, "calc.py 的 add 函数有 bug,修复并验证", out) }()
	for ev := range out {
		if ev.Type == agent.EvToolEnd && ev.Result != nil && ev.Result.IsError {
			t.Fatalf("tool %s failed: %+v", ev.Call.Name, ev.Result.Content)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "calc.py"))
	if !strings.Contains(string(data), "return a + b") {
		t.Fatalf("file not fixed: %s", data)
	}
	if p.calls != 4 {
		t.Fatalf("provider called %d times, want 4", p.calls)
	}
}
