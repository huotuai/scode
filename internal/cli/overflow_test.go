package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"scode/internal/agent"
	"scode/internal/session"
)

func setupOverflowApp(t *testing.T, srv *httptest.Server) *App {
	t.Helper()
	cfgDir := t.TempDir()
	proj := t.TempDir()
	settings := map[string]any{
		"defaultProvider":  "openai-compat",
		"compactionTokens": -1, // threshold path off: isolate the overflow path
		"keepRecentTokens": 1,  // force a cut in this tiny conversation
		"providers": map[string]any{
			"openai-compat": map[string]any{"apiKey": "k", "baseUrl": srv.URL, "model": "m"},
		},
	}
	sb, _ := json.Marshal(settings)
	os.WriteFile(cfgDir+"/settings.json", sb, 0o644) //nolint:errcheck
	t.Setenv("SCODE_DIR", cfgDir)
	t.Chdir(proj)
	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// Overflow recovery (pi's reactive compaction): a context-overflow
// provider error compacts once and retries the failed turn, instead of
// failing the run. Needs real history — a conversation that fits in
// the kept tail has nothing to summarize (pi's prepareCompaction
// returns undefined there too).
func TestOverflowRecoveryE2E(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		n := atomic.AddInt32(&calls, 1)
		switch {
		case n == 1:
			chatSSE(w, "first done", 10)
		case isSummaryRequest(string(body)):
			chatSSE(w, "SUMMARY: earlier work.", 10)
		case n == 2:
			// The second turn fails with a context overflow.
			w.Header().Set("content-type", "text/event-stream")
			fmt.Fprint(w, "data: {\"error\":{\"type\":\"invalid_request_error\",\"message\":\"context length exceeded\"}}\n\n")
		default:
			chatSSE(w, "recovered", 10)
		}
	}))
	defer srv.Close()

	app := setupOverflowApp(t, srv)
	defer app.Close()

	// Build history: one successful run.
	out := make(chan agent.Event, 256)
	if err := app.Run(context.Background(), out, "first"); err != nil {
		t.Fatal(err)
	}
	for range out {
	}

	out = make(chan agent.Event, 256)
	if err := app.Run(context.Background(), out, "do the thing"); err != nil {
		t.Fatalf("overflow recovery must not fail the run: %v", err)
	}
	for range out {
	}

	if got := atomic.LoadInt32(&calls); got != 4 {
		t.Fatalf("provider calls = %d, want 4 (ok, overflow, summarize, retry)", got)
	}
	// The retry's reply is the transcript tail.
	msgs := app.Tr.Messages()
	tail := msgs[len(msgs)-1]
	if tail.Role != "assistant" || tail.Content[0].Text != "recovered" {
		t.Fatalf("tail = %+v", tail)
	}
	// The compaction marker with a kept-tail index is on disk.
	data, err := os.ReadFile(app.Sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"type":"compaction"`) || !strings.Contains(string(data), `"firstKeptEntryId"`) {
		t.Fatal("compaction marker with firstKeptEntryId missing from session file")
	}
}

// A second overflow after the retry must surface as an error (pi
// attempts the compact-and-retry exactly once).
func TestOverflowRecoveryOnlyOnce(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if atomic.AddInt32(&calls, 1) == 1 {
			chatSSE(w, "first done", 10)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		if isSummaryRequest(string(body)) {
			chunks := []string{
				`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
				`{"choices":[{"index":0,"delta":{"content":"SUMMARY"}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
				`[DONE]`,
			}
			for _, c := range chunks {
				fmt.Fprintf(w, "data: %s\n\n", c)
			}
			return
		}
		fmt.Fprint(w, "data: {\"error\":{\"type\":\"invalid_request_error\",\"message\":\"context length exceeded\"}}\n\n")
	}))
	defer srv.Close()

	app := setupOverflowApp(t, srv)
	defer app.Close()

	out := make(chan agent.Event, 256)
	if err := app.Run(context.Background(), out, "first"); err != nil {
		t.Fatal(err)
	}
	for range out {
	}

	out = make(chan agent.Event, 256)
	err := app.Run(context.Background(), out, "boom")
	for range out {
	}
	if err == nil || !isOverflowError(err) {
		t.Fatalf("second overflow must surface, got %v", err)
	}
}

// Manual /compact forces compaction below the threshold, with custom
// focus instructions forwarded to the summary call.
func TestManualCompactCommand(t *testing.T) {
	var sawCustom atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if isSummaryRequest(string(body)) {
			if strings.Contains(string(body), "focus on the DB layer") {
				sawCustom.Store(true)
			}
			chatSSE(w, "SUMMARY: manual.", 10)
			return
		}
		chatSSE(w, "ok", 10)
	}))
	defer srv.Close()

	cfgDir := t.TempDir()
	proj := t.TempDir()
	settings := map[string]any{
		"defaultProvider":  "openai-compat",
		"compactionTokens": -1,
		"keepRecentTokens": 1,
		"providers": map[string]any{
			"openai-compat": map[string]any{"apiKey": "k", "baseUrl": srv.URL, "model": "m"},
		},
	}
	sb, _ := json.Marshal(settings)
	os.WriteFile(cfgDir+"/settings.json", sb, 0o644) //nolint:errcheck
	t.Setenv("SCODE_DIR", cfgDir)
	t.Chdir(proj)

	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	// Two runs of history so the main summary has content (pi forwards
	// custom instructions only to the main summary call).
	for _, prompt := range []string{"work one", "work two"} {
		out := make(chan agent.Event, 256)
		if err := app.Run(context.Background(), out, prompt); err != nil {
			t.Fatal(err)
		}
		for range out {
		}
	}

	_, done, err := app.Command("/compact focus on the DB layer")
	if err != nil || done {
		t.Fatalf("command: done=%v err=%v", done, err)
	}
	if !sawCustom.Load() {
		t.Fatal("custom instructions did not reach the summary call")
	}
	if a := app.Tr.Messages(); len(a) != 3 { // system + summary + kept assistant
		t.Fatalf("transcript after /compact = %d, want 3", len(a))
	}
}

// pi's prepareCompaction guard: a second compaction directly on top of
// a marker (no new conversation since) is a no-op, not an empty update.
func TestCompactOnTopOfMarkerIsNoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if isSummaryRequest(string(body)) {
			chatSSE(w, "SUMMARY: one.", 10)
			return
		}
		chatSSE(w, "ok", 10)
	}))
	defer srv.Close()

	cfgDir := t.TempDir()
	proj := t.TempDir()
	settings := map[string]any{
		"defaultProvider":  "openai-compat",
		"compactionTokens": -1,
		"keepRecentTokens": 1,
		"providers": map[string]any{
			"openai-compat": map[string]any{"apiKey": "k", "baseUrl": srv.URL, "model": "m"},
		},
	}
	sb, _ := json.Marshal(settings)
	os.WriteFile(cfgDir+"/settings.json", sb, 0o644) //nolint:errcheck
	t.Setenv("SCODE_DIR", cfgDir)
	t.Chdir(proj)

	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	out := make(chan agent.Event, 256)
	if err := app.Run(context.Background(), out, "work"); err != nil {
		t.Fatal(err)
	}
	for range out {
	}

	if _, _, err := app.Command("/compact"); err != nil {
		t.Fatal(err)
	}
	markers := 0
	for _, e := range app.entries {
		if e.Compaction != nil {
			markers++
		}
	}
	if markers != 1 {
		t.Fatalf("markers = %d, want 1", markers)
	}
	// Second /compact immediately: no new conversation → no-op.
	if _, _, err := app.Command("/compact"); err != nil {
		t.Fatal(err)
	}
	markers = 0
	for _, e := range app.entries {
		if e.Compaction != nil {
			markers++
		}
	}
	if markers != 1 {
		t.Fatalf("markers after redundant /compact = %d, want 1", markers)
	}
}

// Split turn (pi): when the cut lands mid-turn, the main summary stops
// at the turn start and the turn prefix gets its own checkpoint —
// stored as "Turn Context (split turn)" in the marker.
func TestCompactSplitTurn(t *testing.T) {
	var summaryCalls int32
	var sawPrefixPrompt atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if isSummaryRequest(string(body)) {
			atomic.AddInt32(&summaryCalls, 1)
			if strings.Contains(string(body), "earlier context from an ongoing conversation") {
				sawPrefixPrompt.Store(true)
				chatSSE(w, "PREFIX CHECKPOINT", 10)
				return
			}
			chatSSE(w, "MAIN SUMMARY", 10)
			return
		}
		chatSSE(w, "ok", 10)
	}))
	defer srv.Close()

	cfgDir := t.TempDir()
	proj := t.TempDir()
	settings := map[string]any{
		"defaultProvider":  "openai-compat",
		"compactionTokens": -1,
		"keepRecentTokens": 1,
		"providers": map[string]any{
			"openai-compat": map[string]any{"apiKey": "k", "baseUrl": srv.URL, "model": "m"},
		},
	}
	sb, _ := json.Marshal(settings)
	os.WriteFile(cfgDir+"/settings.json", sb, 0o644) //nolint:errcheck
	t.Setenv("SCODE_DIR", cfgDir)
	t.Chdir(proj)

	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	// Two turns; the cut lands on the second assistant message (mid-turn).
	for _, prompt := range []string{"first task", "second task"} {
		out := make(chan agent.Event, 256)
		if err := app.Run(context.Background(), out, prompt); err != nil {
			t.Fatal(err)
		}
		for range out {
		}
	}

	if _, _, err := app.Command("/compact"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&summaryCalls); got != 2 {
		t.Fatalf("summary calls = %d, want 2 (main + turn prefix)", got)
	}
	if !sawPrefixPrompt.Load() {
		t.Fatal("turn-prefix summarization prompt not used")
	}
	var marker *session.CompactionEntry
	for _, e := range app.entries {
		if e.Compaction != nil {
			marker = e.Compaction
		}
	}
	if marker == nil {
		t.Fatal("no compaction marker")
	}
	if !strings.Contains(marker.Summary, "MAIN SUMMARY") ||
		!strings.Contains(marker.Summary, "**Turn Context (split turn):**") ||
		!strings.Contains(marker.Summary, "PREFIX CHECKPOINT") {
		t.Fatalf("combined summary = %q", marker.Summary)
	}
}
