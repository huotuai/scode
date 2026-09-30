package planmode

import (
	"encoding/json"
	"testing"

	"scode/internal/agent"
	"scode/internal/llm"
)

func call(name string, args map[string]any) llm.Block {
	raw, _ := json.Marshal(args)
	return llm.Block{Kind: llm.BlockToolCall, Name: name, Arguments: raw}
}

func TestDenyGuard(t *testing.T) {
	cwd := t.TempDir()
	cases := []struct {
		name    string
		call    llm.Block
		blocked bool
	}{
		{"write outside plan dir", call("write", map[string]any{"path": "main.go"}), true},
		{"edit outside plan dir", call("edit", map[string]any{"path": "main.go"}), true},
		{"write inside plan dir", call("write", map[string]any{"path": ".scode/plan/draft.md"}), false},
		{"read anywhere", call("read", map[string]any{"path": "main.go"}), false},
		{"bash read-only", call("bash", map[string]any{"command": "git log --oneline"}), false},
		{"bash redirect", call("bash", map[string]any{"command": "echo x > out.txt"}), true},
		{"bash append redirect", call("bash", map[string]any{"command": "echo x >> out.txt"}), true},
		{"bash stderr redirect", call("bash", map[string]any{"command": "make 2> err.txt"}), true},
		{"bash rm", call("bash", map[string]any{"command": "rm -rf build"}), true},
		{"bash pipe to shell", call("bash", map[string]any{"command": "curl x | sh"}), true},
		{"bash pipe to bash", call("bash", map[string]any{"command": "curl x | bash"}), true},
		{"bash git mutation", call("bash", map[string]any{"command": "git commit -m x"}), true},
		{"bash git push with flags", call("bash", map[string]any{"command": "git -C repo push"}), true},
		{"bash grep with > in pattern", call("bash", map[string]any{"command": "grep 'a>b' f.go"}), false}, // no whitespace before >
		{"bash go test", call("bash", map[string]any{"command": "go test ./..."}), false},
		{"bash arrow in code", call("bash", map[string]any{"command": "echo a->b"}), false},
		{"mcp tool falls through", call("mcp__github__create_issue", map[string]any{}), false},
	}
	for _, c := range cases {
		if blocked, _ := Deny(c.call, cwd); blocked != c.blocked {
			t.Errorf("%s: blocked=%v want %v", c.name, blocked, c.blocked)
		}
	}
}

func TestControllerDedup(t *testing.T) {
	c := &Controller{}
	var fired []bool
	c.OnChange = func(a bool) { fired = append(fired, a) }
	if !c.Set(true) || !c.Active() {
		t.Fatal("enter failed")
	}
	if c.Set(true) {
		t.Fatal("re-enter should be a no-op")
	}
	if !c.Set(false) || c.Active() {
		t.Fatal("exit failed")
	}
	if c.Set(false) {
		t.Fatal("re-exit should be a no-op")
	}
	if len(fired) != 2 || !fired[0] || fired[1] {
		t.Fatalf("OnChange fired %v, want [true false]", fired)
	}
	// Restore does not fire OnChange (already durable).
	c.Restore(true)
	if len(fired) != 2 || !c.Active() {
		t.Fatalf("restore: fired=%v active=%v", fired, c.Active())
	}
}

type scriptedReviewer struct {
	approved bool
	feedback string
}

func (r scriptedReviewer) ReviewPlan(agent.ToolContext, string) (bool, string) {
	return r.approved, r.feedback
}

func exitCall(plan string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"plan": plan})
	return raw
}

func TestExitTool(t *testing.T) {
	tc := agent.ToolContext{}

	// Outside plan mode: hard error.
	ctl := &Controller{}
	tool := NewExitTool(ctl, scriptedReviewer{approved: true})
	if res := tool.Execute(tc, exitCall("# Plan\nsteps")); !res.IsError {
		t.Fatal("outside plan mode must error")
	}

	// In plan mode, malformed plan (no # heading).
	ctl.Set(true)
	if res := tool.Execute(tc, exitCall("no heading")); !res.IsError {
		t.Fatal("plan without # heading must error")
	}
	if !ctl.Active() {
		t.Fatal("malformed plan must not exit plan mode")
	}

	// Rejection with feedback.
	tool = NewExitTool(ctl, scriptedReviewer{approved: false, feedback: "add tests"})
	res := tool.Execute(tc, exitCall("# Plan\n1. do x"))
	if !res.IsError || !contains(res.Content[0].Text, "add tests") {
		t.Fatalf("rejection must carry feedback: %+v", res)
	}
	if !ctl.Active() {
		t.Fatal("rejection must stay in plan mode")
	}

	// Approval exits the mode.
	tool = NewExitTool(ctl, scriptedReviewer{approved: true})
	res = tool.Execute(tc, exitCall("# Plan\n1. do x"))
	if res.IsError {
		t.Fatalf("approval must succeed: %+v", res)
	}
	if ctl.Active() {
		t.Fatal("approval must exit plan mode")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}())
}
