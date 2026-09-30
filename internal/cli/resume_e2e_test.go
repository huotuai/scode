package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"scode/internal/agent"
	"scode/internal/session"
)

// Regression for the resume-file bug: a session resumed repeatedly must
// stay resumable and self-contained (previously each resume spun off a
// header-only "-r" file whose later load failed with "leading message
// must be system").
func TestResumeChainE2E(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		chatSSE(w, fmt.Sprintf("reply %d", n), 100)
	}))
	defer srv.Close()

	cfgDir := t.TempDir()
	proj := t.TempDir()
	settings := map[string]any{
		"defaultProvider":  "openai-compat",
		"compactionTokens": -1, // keep the test focused on resume, not compaction
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

	// Generation 0: create + run.
	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	id := app.Sess.Header().ID
	out := make(chan agent.Event, 256)
	if err := app.Run(context.Background(), out, "first"); err != nil {
		t.Fatal(err)
	}
	for range out {
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}

	// Generations 1 and 2: resume the SAME id twice.
	for _, prompt := range []string{"second", "third"} {
		app, err := Setup(Options{Resume: id})
		if err != nil {
			t.Fatalf("resume %q: %v", prompt, err)
		}
		out := make(chan agent.Event, 256)
		if err := app.Run(context.Background(), out, prompt); err != nil {
			t.Fatalf("run %q: %v", prompt, err)
		}
		for range out {
		}
		if err := app.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// One file, no -r siblings, full history on disk.
	ids, err := func() ([]string, error) {
		store := session.DefaultRoot(cfgDir, proj)
		entries, err := os.ReadDir(store)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return out, nil
	}()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != id+".jsonl" {
		t.Fatalf("session files = %v, want exactly [%s.jsonl]", ids, id)
	}
}

// Regression: session-cumulative usage (and with it the cache-hit rate)
// must survive resume — the historical assistant usages on disk fold
// back into a.spent instead of restarting at zero.
func TestResumeFoldsUsageE2E(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		chunks := []string{
			`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%s}}]}`, mustMarshalString("ok")),
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":80}}}`,
			`[DONE]`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
	}))
	defer srv.Close()

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

	// One turn: input=20 cacheRead=80 output=10.
	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	id := app.Sess.Header().ID
	out := make(chan agent.Event, 256)
	if err := app.Run(context.Background(), out, "first"); err != nil {
		t.Fatal(err)
	}
	for range out {
	}
	r := app.UsageReport()
	if r.Input != 20 || r.CacheRead != 80 || r.Output != 10 {
		t.Fatalf("live usage = in %d cacheRead %d out %d, want 20/80/10", r.Input, r.CacheRead, r.Output)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}

	// Resume WITHOUT a new turn: the totals must fold from disk.
	app2, err := Setup(Options{Resume: id})
	if err != nil {
		t.Fatal(err)
	}
	r = app2.UsageReport()
	if r.Input != 20 || r.CacheRead != 80 || r.Output != 10 {
		t.Fatalf("resumed usage = in %d cacheRead %d out %d, want folded 20/80/10", r.Input, r.CacheRead, r.Output)
	}

	// A second turn on the resumed app ADDS to the folded totals (the
	// historical turn is never double-counted).
	out2 := make(chan agent.Event, 256)
	if err := app2.Run(context.Background(), out2, "second"); err != nil {
		t.Fatal(err)
	}
	for range out2 {
	}
	r = app2.UsageReport()
	if r.Input != 40 || r.CacheRead != 160 || r.Output != 20 {
		t.Fatalf("post-run usage = in %d cacheRead %d out %d, want 40/160/20", r.Input, r.CacheRead, r.Output)
	}
	if err := app2.Close(); err != nil {
		t.Fatal(err)
	}
}
