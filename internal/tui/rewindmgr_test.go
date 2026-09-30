package tui

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"scode/internal/checkpoint"
)

// The /rewind overlay: the session's checkpoints listed newest first,
// enter twice restores the files, esc closes.
func TestRewindOverlayFlow(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	ui := make(chan any, 16)
	m := newModel(app, ui)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(model)

	// Two checkpoints through the real store: an overwrite and a
	// not-yet-existing file (the created-file case).
	target := filepath.Join(app.CWD, "demo.txt")
	os.WriteFile(target, []byte("v1\n"), 0o644) //nolint:errcheck
	store := checkpoint.NewStore(app.CfgDir)
	if _, err := store.Capture(app.Sess.Header().ID, "edit", "demo.txt", []string{target}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(target, []byte("v2\n"), 0o644) //nolint:errcheck
	if _, err := store.Capture(app.Sess.Header().ID, "edit", "demo.txt", []string{target}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(target, []byte("v3\n"), 0o644) //nolint:errcheck

	m.runCommand("/rewind")
	if !m.rewindOpen || !m.inputHidden() {
		t.Fatal("overlay did not open (or did not hide the input)")
	}
	view := plain(m.rewindView())
	if !strings.Contains(view, "c002") || !strings.Contains(view, "demo.txt") {
		t.Fatalf("checkpoints missing from the list:\n%s", view)
	}
	// Newest first.
	if strings.Index(view, "c002") > strings.Index(view, "c001") {
		t.Fatalf("list not newest-first:\n%s", view)
	}

	// First enter arms, esc disarms without touching anything.
	m = press(t, m, tea.KeyEnter)
	if !m.rewindConfirm {
		t.Fatal("enter did not arm the confirm")
	}
	m = press(t, m, tea.KeyEscape)
	if m.rewindConfirm {
		t.Fatal("esc did not disarm")
	}

	// Enter twice restores the highlighted (newest) checkpoint: v3 → v2.
	m = press(t, m, tea.KeyEnter)
	m = press(t, m, tea.KeyEnter)
	if m.toast == nil || !strings.Contains(m.toast.text, "已回滚到 c002") {
		t.Fatalf("restore toast missing: %+v", m.toast)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "v2\n" {
		t.Fatalf("restore wrote %q", data)
	}

	m = press(t, m, tea.KeyEscape)
	if m.rewindOpen {
		t.Fatal("esc did not close the overlay")
	}
	if m.input.Value() != "" {
		t.Fatalf("input polluted: %q", m.input.Value())
	}
}
