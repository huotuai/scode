package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"scode/internal/agent"
)

// /new swaps the app onto a fresh session in place: the conversation
// state resets, the old log stays resumable on disk, and subsequent
// turns land in the NEW file.
func TestNewSessionResetsInPlace(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		chatSSE(w, fmt.Sprintf("reply %d", n), 100)
	}))
	defer srv.Close()
	setupTestApp(t, srv)

	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck

	run := func(prompt string) {
		t.Helper()
		out := make(chan agent.Event, 256)
		if err := app.Run(context.Background(), out, prompt); err != nil {
			t.Fatalf("run %q: %v", prompt, err)
		}
		for range out {
		}
	}
	run("first")
	oldID := app.Sess.Header().ID
	oldPath := app.Sess.Path
	trBefore := app.Tr.Len()

	gotOld, newID, err := app.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if gotOld != oldID {
		t.Fatalf("old id = %q, want %q", gotOld, oldID)
	}
	if newID == "" || newID == oldID {
		t.Fatalf("new id %q must differ from %q", newID, oldID)
	}

	// Conversation state is reset: the transcript is back to the
	// request-time system prefix, the entry mirror and plan are empty.
	if app.Tr.Len() >= trBefore {
		t.Fatalf("transcript not reset: len %d, was %d", app.Tr.Len(), trBefore)
	}
	if len(app.entries) != 0 {
		t.Fatalf("entries = %d, want 0", len(app.entries))
	}
	if _, ok := app.PlanState(); ok {
		t.Fatal("plan state should be empty after /new")
	}

	// The old log is untouched and resumable.
	rec, err := app.Store.Load(oldID)
	if err != nil {
		t.Fatalf("old session %s no longer loadable: %v", oldID, err)
	}
	if len(rec.Path()) == 0 {
		t.Fatal("old session lost its conversation")
	}

	// A turn after /new persists into the NEW file, not the old one.
	before := fileSize(t, oldPath)
	run("second")
	if app.Sess.Header().ID != newID {
		t.Fatalf("standing session = %q, want %q", app.Sess.Header().ID, newID)
	}
	if got := fileSize(t, oldPath); got != before {
		t.Fatalf("old session file grew: %d -> %d", before, got)
	}
	rec2, err := app.Store.Load(newID)
	if err != nil {
		t.Fatalf("new session not loadable: %v", err)
	}
	if len(rec2.Path()) == 0 {
		t.Fatal("new session did not record the post-/new turn")
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}
