package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// newPlanModel returns a model with a pending plan review over a long
// body (many wrapped lines, far beyond the visible budget).
func newPlanModel(t *testing.T) model {
	t.Helper()
	m := newTestModel()
	m.width, m.height = 80, 24
	m.input.Focus() // an unfocused textarea swallows keystrokes
	m.pending = &approvalRequest{
		kind:   "plan",
		title:  "plan review",
		body:   strings.TrimSuffix(strings.Repeat("第一行计划内容说明。\n", 60), "\n"),
		hint:   "[y] 批准并执行",
		answer: make(chan string, 1),
	}
	m.resize()
	return m
}

// keyPress runs keystrokes through the model (test shorthand).
func keyPress(t *testing.T, m model, codes ...rune) model {
	t.Helper()
	for _, c := range codes {
		tm, _ := m.handleKey(tea.KeyPressMsg{Code: c})
		m = tm.(model)
	}
	return m
}

// The plan review box: capped height (a long plan never swallows the
// screen), a scroll-position footer, buttons instead of letter answers.
func TestPlanApprovalWindow(t *testing.T) {
	m := newPlanModel(t)

	// The box is capped: title + budget body lines + footer + spacer +
	// buttons + hint + the frame's two padding rows, well under the
	// full 24-row window.
	view := plain(m.approvalView())
	h := strings.Count(view, "\n") + 1
	_, budget := m.planWindow()
	if h > budget+8 {
		t.Fatalf("plan box = %d lines (budget %d), too tall:\n%s", h, budget, view)
	}
	if !strings.Contains(view, "计划确认") || !strings.Contains(view, "共") {
		t.Fatalf("plan box missing chrome:\n%s", view)
	}
	for _, b := range planButtons {
		if !strings.Contains(view, b) {
			t.Fatalf("button %q missing:\n%s", b, view)
		}
	}

	// Scrolling: ↓ advances the window (footer numbers move), clamps at
	// the end; ↑ clamps at zero.
	m = keyPress(t, m, tea.KeyDown, tea.KeyDown)
	view = plain(m.approvalView())
	if !strings.Contains(view, "第 3–") {
		t.Fatalf("scroll did not advance:\n%s", view)
	}
	for range 100 {
		m = keyPress(t, m, tea.KeyDown)
	}
	lines, budget := m.planWindow()
	view = plain(m.approvalView())
	if !strings.Contains(view, fmt.Sprintf("第 %d–%d 行,共 %d 行", len(lines)-budget+1, len(lines), len(lines))) {
		t.Fatalf("scroll did not clamp at the end:\n%s", view)
	}
	for range 100 {
		m = keyPress(t, m, tea.KeyUp)
	}
	if m.planScroll != 0 {
		t.Fatalf("scroll did not clamp at zero: %d", m.planScroll)
	}
}

// Button navigation and answers: ←/→ move the focus, Enter sends the
// focused action; a plain letter NEVER answers (mistouch guard) — it
// just drafts feedback.
func TestPlanApprovalButtons(t *testing.T) {
	// Focus starts on 批准执行; Enter approves.
	m := newPlanModel(t)
	ch := m.pending.answer
	m = keyPress(t, m, tea.KeyEnter)
	if ans := <-ch; ans != "y" {
		t.Fatalf("approve answer = %q, want y", ans)
	}
	if m.pending != nil {
		t.Fatal("pending survived the answer")
	}

	// ←/→ wrap around: → → lands on 拒绝.
	m = newPlanModel(t)
	ch = m.pending.answer
	m = keyPress(t, m, tea.KeyRight, tea.KeyRight)
	if m.planBtn != 2 {
		t.Fatalf("focus = %d, want 2 (拒绝)", m.planBtn)
	}
	m = keyPress(t, m, tea.KeyEnter)
	if ans := <-ch; ans != "n" {
		t.Fatalf("reject answer = %q, want n", ans)
	}

	// 打回修订 uses the typed draft as feedback.
	m = newPlanModel(t)
	ch = m.pending.answer
	m = keyPress(t, m, tea.KeyRight)
	m = typeKeys(t, m, "第三步改成先写测试")
	m = keyPress(t, m, tea.KeyEnter)
	if ans := <-ch; ans != "feedback:第三步改成先写测试" {
		t.Fatalf("revise answer = %q", ans)
	}

	// 打回修订 with an empty draft sends a generic note.
	m = newPlanModel(t)
	ch = m.pending.answer
	m = keyPress(t, m, tea.KeyRight)
	m = keyPress(t, m, tea.KeyEnter)
	if ans := <-ch; ans != "feedback:请修订该计划" {
		t.Fatalf("generic revise answer = %q", ans)
	}

	// Typing "y" does NOT answer: it lands in the input as feedback, and
	// Enter sends it as revision feedback.
	m = newPlanModel(t)
	ch = m.pending.answer
	m = typeKeys(t, m, "y")
	select {
	case ans := <-ch:
		t.Fatalf("y keystroke answered the prompt: %q", ans)
	default:
	}
	m = keyPress(t, m, tea.KeyEnter)
	if ans := <-ch; ans != "feedback:y" {
		t.Fatalf("typed-y Enter answer = %q, want feedback:y", ans)
	}

	// Esc still denies outright.
	m = newPlanModel(t)
	ch = m.pending.answer
	m = keyPress(t, m, tea.KeyEscape)
	if ans := <-ch; ans != "n" {
		t.Fatalf("esc answer = %q, want n", ans)
	}
}
