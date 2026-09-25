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
