package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scode/internal/agent"
)

// setupTestApp points SCODE_DIR at a temp config dir with a fake
// openai-compat provider behind srv, in a temp working directory.
func setupTestApp(t *testing.T, srv *httptest.Server) {
	t.Helper()
	cfgDir := t.TempDir()
	proj := t.TempDir()
	settings := map[string]any{
		"defaultProvider":  "openai-compat",
		"compactionTokens": -1,
		"providers": map[string]any{
			"openai-compat": map[string]any{
				"apiKey":  "test",
				"baseUrl": srv.URL,
				"model":   "test-model",
			},
		},
	}
	sb, _ := json.Marshal(settings)
	os.WriteFile(filepath.Join(cfgDir, "settings.json"), sb, 0o644) //nolint:errcheck
	t.Setenv("SCODE_DIR", cfgDir)
	t.Chdir(proj)
}

// Regression: resuming a header-only session (crash between Create and
// the first append) panicked with index-out-of-range on msgs[0]. With
// conversation-only storage the transcript is just the freshly built
// system message, and the run proceeds normally.
func TestResumeHeaderOnlySession(t *testing.T) {
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
	id := app.Sess.Header().ID
	path := app.Sess.Path
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate the crash: truncate the file to just the header line.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	header, _, _ := strings.Cut(string(data), "\n")
	if err := os.WriteFile(path, []byte(header+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	app, err = Setup(Options{Resume: id})
	if err != nil {
		t.Fatalf("resume header-only session: %v", err)
	}
	out := make(chan agent.Event, 256)
	if err := app.Run(context.Background(), out, "hello"); err != nil {
		t.Fatal(err)
	}
	for range out {
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}

	// The file holds the conversation only (system messages are never
	// stored): user + assistant.
	rec, err := app.Store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	msgs := rec.Project()
	if len(msgs) != 2 {
		t.Fatalf("projected messages = %d, want 2", len(msgs))
	}
}

// Regression: persistence is incremental (durable mid-run) and a failed
// write must not be skipped — the watermark stays put so the next sync
// retries the same messages.
func TestPersistWatermarkRetriesAfterFailure(t *testing.T) {
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
	id := app.Sess.Header().ID

	// Break persistence: writes to the closed file fail.
	if err := app.Sess.Close(); err != nil {
		t.Fatal(err)
	}
	out := make(chan agent.Event, 256)
	err = app.Run(context.Background(), out, "first")
	for range out {
	}
	if err == nil {
		t.Fatal("expected a persist error")
	}
	if app.persisted != 2 { // position 0 (system) is skipped, and the setup-time
		// sandbox section delta is the message at 1; the user write fails
		t.Fatalf("persisted = %d, want 2 (watermark must not advance on failure)", app.persisted)
	}
	if app.Tr.Len() != 4 { // system + sandbox section delta + user + assistant, all retained in memory
		t.Fatalf("transcript len = %d, want 4", app.Tr.Len())
	}

	// Reopen the file; the next run must flush the backlog first.
	sess, err := app.Store.OpenForAppend(id)
	if err != nil {
		t.Fatal(err)
	}
	app.Sess = sess
	out = make(chan agent.Event, 256)
	if err := app.Run(context.Background(), out, "second"); err != nil {
		t.Fatal(err)
	}
	for range out {
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}

	rec, err := app.Store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	// system is request-time only: (user+assistant)×2 — nothing was
	// lost despite the mid-session persist failure.
	if got := len(rec.Project()); got != 4 {
		t.Fatalf("projected messages = %d, want 4", got)
	}
}

// Incremental durability: a finished turn must be on disk before the
// run returns — including the error turn of a failing provider.
func TestFailedRunPersistsErrorTurn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, "data: {\"error\":{\"type\":\"quota\",\"message\":\"billing exhausted\"}}\n\n")
	}))
	defer srv.Close()
	setupTestApp(t, srv)

	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	id := app.Sess.Header().ID
	out := make(chan agent.Event, 256)
	if err := app.Run(context.Background(), out, "boom"); err == nil {
		t.Fatal("expected run error")
	}
	for range out {
	}
	app.Close() //nolint:errcheck

	rec, err := app.Store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	var sawError bool
	for _, e := range rec.Path() {
		if e.Msg != nil && e.Msg.StopReason == "error" {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("error turn not persisted to the session file")
	}
}
