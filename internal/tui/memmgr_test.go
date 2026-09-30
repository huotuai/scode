package tui

import (
	"net/http"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"scode/internal/memory"
)

func seedMemories(t *testing.T, m *model) *memory.Store {
	t.Helper()
	s := memory.Open(m.app.CfgDir, m.app.CWD)
	s.Apply([]memory.Op{ //nolint:errcheck — best-effort seed
		{Op: "add", Category: "project", Content: "构建用 build.bat"},
		{Op: "add", Category: "preference", Content: "回复用中文"},
	}, "seed")
	return s
}

// The /memory overlay: entries listed by category, s toggles the
// relevance pick (persisted), d twice deletes, e fires the async
// extraction, esc closes.
func TestMemoryOverlayFlow(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	ui := make(chan any, 64)
	m := newModel(app, ui)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(model)
	store := seedMemories(t, &m)

	m.runCommand("/memory")
	if !m.memOpen || !m.inputHidden() {
		t.Fatal("overlay did not open (or did not hide the input)")
	}
	view := plain(m.memoryView())
	if !strings.Contains(view, "构建用 build.bat") || !strings.Contains(view, "偏好") {
		t.Fatalf("entries missing:\n%s", view)
	}

	// s toggles the selection of the highlighted entry (persisted).
	m = press(t, m, 's')
	entries, _ := store.Load()
	if !entries[0].Selected {
		t.Fatalf("selection not persisted: %+v", entries)
	}
	if view = plain(m.memoryView()); !strings.Contains(view, "● 项目 · 构建") {
		t.Fatalf("selected marker missing:\n%s", view)
	}

	// d twice deletes the highlighted entry.
	m = press(t, m, 'd')
	if !m.memConfirmDel {
		t.Fatal("first d did not arm")
	}
	m = press(t, m, 'd')
	if entries, _ = store.Load(); len(entries) != 1 {
		t.Fatalf("delete left %+v", entries)
	}

	// e fires the async extraction — the note appears immediately and
	// the summary lands on the ui channel.
	m = press(t, m, 'e')
	if txt := blocksText(&m); !strings.Contains(txt, "记忆提取中") {
		t.Fatalf("extraction note missing:\n%s", txt)
	}
	select {
	case msg := <-ui:
		l, ok := msg.(logMsg)
		if !ok || strings.TrimSpace(string(l)) == "" {
			// The mock has no conversation to extract — the no-op summary
			// or an error is a fine outcome; what matters is delivery.
			t.Fatalf("extraction result = %v", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("extraction result never arrived")
	}

	m = press(t, m, tea.KeyEscape)
	if m.memOpen {
		t.Fatal("esc did not close the overlay")
	}
	if m.input.Value() != "" {
		t.Fatalf("input polluted: %q", m.input.Value())
	}
}
