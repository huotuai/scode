package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"scode/internal/agent"
	"scode/internal/llm"
	"scode/internal/permission"
)

// Tool/sandbox asks are button-driven: the box shows the action
// buttons, the arrows move the focus, Enter activates the focused
// button, Esc denies, and the letter keys stay as accelerators.
func TestApprovalButtons(t *testing.T) {
	call := llm.Block{Kind: llm.BlockToolCall, Name: "bash", Arguments: []byte(`{"command":"rm -rf /tmp/x"}`)}
	rule := &permission.Rule{Raw: "bash"}

	// The box shows the buttons and the navigation hint.
	m, resCh := awaitToolAsk(t, call, rule)
	view := plain(m.approvalView())
	for _, want := range []string{"工具审批", "允许一次", "本会话允许", "本项目允许", "拒绝", "Enter 确认", "Esc 拒绝"} {
		if !strings.Contains(view, want) {
			t.Fatalf("approval box missing %q:\n%s", want, view)
		}
	}
	// Default focus is the first button; the arrows move it, wrapping.
	tm, _ := m.handleKey(tea.KeyPressMsg{Code: tea.KeyLeft})
	m = tm.(model)
	if m.approvalBtn != len(approvalButtons("tool"))-1 {
		t.Fatalf("left from the first button did not wrap to the last: %d", m.approvalBtn)
	}
	tm, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyRight})
	m = tm.(model)
	if m.approvalBtn != 0 {
		t.Fatalf("right from the last button did not wrap to the first: %d", m.approvalBtn)
	}
	// →→ onto 本项目允许, Enter activates it (the ask unblocks with a
	// project rule) and the transcript echoes the button label.
	tm, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyRight})
	m = tm.(model)
	tm, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyRight})
	m = tm.(model)
	if m.approvalBtn != 2 {
		t.Fatalf("focus = %d, want 2", m.approvalBtn)
	}
	tm, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = tm.(model)
	if m.pending != nil {
		t.Fatal("Enter did not resolve the ask")
	}
	select {
	case got := <-resCh:
		if got != "p" {
			t.Fatalf("answer = %q, want p", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ask did not unblock")
	}
	if last := plain(m.blocks[len(m.blocks)-1].rendered); !strings.Contains(last, "本项目允许") {
		t.Fatalf("echo missing the button label: %q", last)
	}

	// A letter key still answers directly (accelerator).
	m, resCh = awaitToolAsk(t, call, rule)
	if !m.answerKey("n") {
		t.Fatal("answerKey(n) rejected")
	}
	select {
	case got := <-resCh:
		if got != "n" {
			t.Fatalf("answer = %q, want n", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ask did not unblock")
	}

	// Esc denies.
	m, resCh = awaitToolAsk(t, call, rule)
	tm, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = tm.(model)
	select {
	case got := <-resCh:
		if got != "n" {
			t.Fatalf("esc answer = %q, want n", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ask did not unblock")
	}

	// The sandbox ask has its own three buttons.
	m, resCh = awaitSandboxAsk(t)
	view = plain(m.approvalView())
	for _, want := range []string{"沙箱提权审批", "仅本次", "本会话生效", "拒绝"} {
		if !strings.Contains(view, want) {
			t.Fatalf("sandbox box missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "本项目允许") {
		t.Fatalf("sandbox box shows a tool-only button:\n%s", view)
	}
	// ↓ onto 本会话生效, Enter.
	tm, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	m = tm.(model)
	tm, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = tm.(model)
	select {
	case got := <-resCh:
		if got != "a" {
			t.Fatalf("answer = %q, want a", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ask did not unblock")
	}
}

// awaitToolAsk starts a tool approval ask and installs it on a fresh
// sized model; the channel reports the raw answer letter.
func awaitToolAsk(t *testing.T, call llm.Block, rule *permission.Rule) (model, <-chan string) {
	t.Helper()
	ui := make(chan any, 4)
	a := newApprover(ui)
	resCh := make(chan string, 1)
	go func() {
		r := a.Ask(context.Background(), call, rule, "bash(rm -rf /tmp/x)")
		ans := "n"
		switch {
		case r.ProjectRule != "":
			ans = "p"
		case r.SessionRule != "":
			ans = "a"
		case r.Allow:
			ans = "y"
		}
		resCh <- ans
	}()
	m := newTestModel()
	m.width, m.height = 100, 30
	select {
	case msg := <-ui:
		m.pending = msg.(approvalMsg).req
		m.approvalBtn = 0
	case <-time.After(2 * time.Second):
		t.Fatal("no approval request reached the UI stream")
	}
	return m, resCh
}

// awaitSandboxAsk starts a sandbox escalation ask likewise.
func awaitSandboxAsk(t *testing.T) (model, <-chan string) {
	t.Helper()
	ui := make(chan any, 4)
	a := newApprover(ui)
	resCh := make(chan string, 1)
	go func() {
		r := a.ReviewSandboxEscalation(context.Background(), agent.EscalationRequest{
			Tool: "bash", CurrentMode: "workspace-write", RequestedMode: "danger-full-access",
			Detail: "write /etc/x", Justification: "需要",
		})
		ans := "n"
		switch {
		case r.Approved && r.ApplyToSession:
			ans = "a"
		case r.Approved:
			ans = "y"
		}
		resCh <- ans
	}()
	m := newTestModel()
	m.width, m.height = 100, 30
	select {
	case msg := <-ui:
		m.pending = msg.(approvalMsg).req
		m.approvalBtn = 0
	case <-time.After(2 * time.Second):
		t.Fatal("no approval request reached the UI stream")
	}
	return m, resCh
}
