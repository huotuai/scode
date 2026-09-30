package tui

import (
	"net/http"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"scode/internal/config"
)

// A keyboard-owning modal hides the input box; the plan review and the
// typing-attached popups (palette, file picker) keep it — their typed
// text is functional input, not dead weight.
func TestInputHiddenBehindModals(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		chatSSE(w, "reply")
	})
	m := newModel(app, make(chan any, 4))
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(model)
	m.input.SetValue("draft")

	// The input box's plain top rule (a full-width dash line with no
	// title — every overlay box embeds a title) is its stable marker;
	// the plan review's box carries the "计划反馈" label instead.
	inputShown := func(mm *model) bool {
		c := stripANSI(mm.View().Content)
		if mm.pending != nil && mm.pending.kind == "plan" {
			return strings.Contains(c, "计划反馈")
		}
		rule := "╭" + strings.Repeat("─", mm.width-2) + "╮"
		return strings.Contains(c, rule)
	}
	// The status bar is the layout's bottom line: assert on the last
	// non-empty row (the badge word alone would false-positive inside
	// the sandbox picker, which renders the same words).
	badge, _ := sandboxBadge(app.SandboxMode())
	statusShown := func(mm *model) bool {
		lines := strings.Split(stripANSI(mm.View().Content), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if l := strings.TrimRight(lines[i], " "); l != "" {
				return strings.Contains(l, badge)
			}
		}
		return false
	}
	if !inputShown(&m) || !statusShown(&m) {
		t.Fatal("input box or status bar missing from the idle layout")
	}

	// Button-driven approval asks (tool/sandbox): hidden, and the
	// viewport height accounts for the reclaimed rows exactly.
	m.pending = &approvalRequest{kind: "tool", title: "approval required (rule \"bash\")",
		body: "  bash command: rm -rf /tmp/x", hint: "[y] [n] [a] [p]", answer: make(chan string, 1)}
	m.resize()
	if inputShown(&m) {
		t.Fatal("input box should hide behind a tool approval")
	}
	if statusShown(&m) {
		t.Fatal("status bar should hide with the input box behind a tool approval")
	}
	if want := 24 - lipgloss.Height(m.approvalView()); m.vp.Height() != want {
		t.Fatalf("viewport height = %d, want %d (height minus the approval box — input and status rows are reclaimed)", m.vp.Height(), want)
	}
	m.pending = nil
	m.resize()
	if !inputShown(&m) || !statusShown(&m) {
		t.Fatal("input box or status bar not restored after the approval")
	}
	if m.input.Value() != "draft" {
		t.Fatalf("input text lost while hidden: %q", m.input.Value())
	}

	// Plan review: the input stays — typed letters are revision feedback.
	m.pending = &approvalRequest{kind: "plan", title: "plan review", body: "1. do the thing",
		hint: "[←/→] [enter]", answer: make(chan string, 1)}
	m.resize()
	if !inputShown(&m) {
		t.Fatal("plan review must keep the input box (typed text = feedback)")
	}
	m.pending = nil
	m.resize()

	// Full managers: hidden. The models manager needs its catalog state
	// (the real opener fetches it); the others open with plain flags.
	modals := []struct {
		name string
		open func(m *model)
	}{
		{"sandbox", func(m *model) { m.sandboxOpen = true }},
		{"model picker", func(m *model) { m.modelOpen = true }},
		{"models manager", func(m *model) {
			m.modelsCat = &config.PresetCatalog{Vendors: []config.PresetVendor{{
				ID: "test", Name: "Test", Protocol: "openai-compat",
			}}}
			m.modelsStage = modelsStageVendor
			m.modelsOpen = true
		}},
		{"mcp manager", func(m *model) { m.mcpOpen = true }},
	}
	for _, mo := range modals {
		mo.open(&m)
		m.resize()
		if inputShown(&m) {
			t.Fatalf("input box should hide behind the %s overlay", mo.name)
		}
		if statusShown(&m) {
			t.Fatalf("status bar should hide behind the %s overlay", mo.name)
		}
		m.sandboxOpen, m.modelOpen, m.modelsOpen, m.mcpOpen = false, false, false, false
		m.resize()
		if !inputShown(&m) || !statusShown(&m) {
			t.Fatalf("input box or status bar not restored after the %s overlay closed", mo.name)
		}
	}

	// Typing-attached popups: the palette and the @ file picker keep it.
	m.input.SetValue("/")
	m.updatePalette()
	if !inputShown(&m) {
		t.Fatal("palette must keep the input box")
	}
	m.input.SetValue("")
	m.updatePalette()
	m.openPicker() //nolint:errcheck
	if !inputShown(&m) {
		t.Fatal("file picker must keep the input box")
	}
	if !statusShown(&m) {
		t.Fatal("file picker must keep the status bar")
	}
}
