package tui

import (
	"net/http"
	"strings"
	"testing"
)

// /new swaps the app onto a fresh session and resets the transcript
// view: the old conversation's blocks are gone, the banner names the
// NEW session id, and a note keeps the old one's resume path.
func TestNewCommandResetsSessionAndView(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	defer app.Close() //nolint:errcheck
	m := newModel(app, make(chan any, 16))

	oldID := app.Sess.Header().ID
	// Stand in for an ongoing conversation's rendered transcript.
	m.appendBlock("✨ hello")
	m.appendBlock("some reply text")
	blocksBefore := len(m.blocks)

	m.runCommand("/new")

	newID := app.Sess.Header().ID
	if newID == oldID {
		t.Fatal("session id did not change on /new")
	}
	if len(m.blocks) >= blocksBefore {
		t.Fatalf("transcript view not reset: %d blocks, was %d", len(m.blocks), blocksBefore)
	}
	if !m.vp.AtBottom() {
		t.Fatal("scroller should be pinned to the fresh transcript")
	}
	var content string
	for _, b := range m.blocks {
		content += plain(b.rendered) + "\n"
	}
	if !strings.Contains(content, newID) {
		t.Fatalf("banner should name the new session %s:\n%s", newID, content)
	}
	if !strings.Contains(content, oldID) {
		t.Fatalf("the note should keep the old session's resume path (%s):\n%s", oldID, content)
	}
	// The old log stays resumable.
	if _, err := app.Store.Load(oldID); err != nil {
		t.Fatalf("old session %s no longer loadable: %v", oldID, err)
	}
}

// A queued /new fires after the in-flight run finishes (the queue
// drain calls runCommand with m.running already false).
func TestNewCommandGuardedWhileRunning(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	defer app.Close() //nolint:errcheck
	m := newModel(app, make(chan any, 16))
	m.running = true
	oldID := app.Sess.Header().ID

	m.runCommand("/new")

	if app.Sess.Header().ID != oldID {
		t.Fatal("/new must not swap the session mid-run")
	}
	last := m.blocks[len(m.blocks)-1]
	if !strings.Contains(plain(last.rendered), "运行中无法开始新会话") {
		t.Fatalf("guard note missing:\n%s", plain(last.rendered))
	}
}
