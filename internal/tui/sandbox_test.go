package tui

import (
	"net/http"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// The /sandbox command: palette advertises it, the command opens the
// picker overlay with all three modes (current one marked), Enter
// applies the highlighted mode and echoes the confirmation, Esc closes
// without switching.
func TestSandboxCommand(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	m := newModel(app, make(chan any, 16))
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	// The palette narrows "/sa" to the command.
	m.input.SetValue("/sa")
	m.updatePalette()
	found := false
	for _, h := range m.paletteHits {
		if h.Name == "/sandbox" {
			found = true
		}
	}
	if !found {
		t.Fatalf("/sandbox missing from palette hits: %+v", m.paletteHits)
	}
	m.input.Reset()
	m.updatePalette()

	// The command opens the picker with every mode and the current one
	// marked.
	m.runCommand("/sandbox")
	if !m.sandboxOpen {
		t.Fatal("picker did not open")
	}
	view := plain(m.sandboxView())
	for _, want := range []string{"只读", "可写", "不限制", "当前", "Esc"} {
		if !strings.Contains(view, want) {
			t.Fatalf("picker view missing %q:\n%s", want, view)
		}
	}
	// The whole view renders with the overlay between picker and input.
	if v := plain(string(m.view().Content)); !strings.Contains(v, "沙箱模式") {
		t.Fatalf("overlay missing from the view:\n%s", v)
	}

	// ↓ lands on danger-full-access (default workspace-write highlights
	// one row above); Enter applies it.
	m2, _ := m.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	m4, _ := m2.(model).handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = m4.(model)
	if m.sandboxOpen {
		t.Fatal("picker still open after Enter")
	}
	if got := app.SandboxMode(); got != "danger-full-access" {
		t.Fatalf("sandbox mode = %q, want danger-full-access", got)
	}
	last := plain(m.blocks[len(m.blocks)-1].rendered)
	if !strings.Contains(last, "沙箱已关闭") {
		t.Fatalf("confirmation not echoed: %q", last)
	}

	// Re-opening highlights the (new) current mode; Esc closes without
	// switching.
	m.runCommand("/sandbox")
	if m.sandboxIdx != 2 {
		t.Fatalf("highlight = %d, want the current mode (2)", m.sandboxIdx)
	}
	m5, _ := m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = m5.(model)
	if m.sandboxOpen {
		t.Fatal("picker still open after Esc")
	}
	if got := app.SandboxMode(); got != "danger-full-access" {
		t.Fatalf("Esc changed the mode: %q", got)
	}

	// Picking the current mode is a quiet no-op close.
	m.runCommand("/sandbox")
	m6, _ := m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}) // highlight already on current
	m = m6.(model)
	if m.sandboxOpen || plain(m.blocks[len(m.blocks)-1].rendered) == "沙箱:只读(禁止文件修改)" {
		t.Fatal("re-picking the current mode should not echo a switch")
	}
}
