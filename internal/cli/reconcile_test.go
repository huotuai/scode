package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scode/internal/agent"
	"scode/internal/config"
	"scode/internal/llm"
	"scode/internal/session"
)

func TestResolveCompactTokens(t *testing.T) {
	cases := []struct {
		name   string
		set    int
		window int
		want   int
	}{
		{"explicit wins", 50_000, 200_000, 50_000},
		{"negative disables", -1, 200_000, -1},
		{"window minus reserve", 0, 200_000, 200_000 - session.DefaultReserveTokens},
		{"tiny window floors at 16k", 0, 20_000, 16_000},
		{"unknown window falls back", 0, 0, session.DefaultCompactionTokens},
	}
	for _, c := range cases {
		got := resolveCompactTokens(&config.Settings{CompactionTokens: c.set}, c.window)
		if got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// Resume rebuilds the prompt from the CURRENT build: an AGENTS.md
// edited between runs applies with no delta system messages and no
// writes to the session file (pi's boundary — the prompt is never
// stored, so there is nothing to reconcile).
func TestResumeRebuildsPrompt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatSSE(w, "ok", 100)
	}))
	defer srv.Close()

	cfgDir := t.TempDir()
	proj := t.TempDir()
	os.WriteFile(filepath.Join(proj, "AGENTS.md"), []byte("use Go 1.26"), 0o644) //nolint:errcheck
	settings := map[string]any{
		"defaultProvider":  "openai-compat",
		"compactionTokens": -1,
		"providers": map[string]any{
			"openai-compat": map[string]any{"apiKey": "k", "baseUrl": srv.URL, "model": "m"},
		},
	}
	sb, _ := json.Marshal(settings)
	os.WriteFile(filepath.Join(cfgDir, "settings.json"), sb, 0o644) //nolint:errcheck
	t.Setenv("SCODE_DIR", cfgDir)
	t.Chdir(proj)

	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	id := app.Sess.Header().ID
	out := make(chan agent.Event, 64)
	if err := app.Run(context.Background(), out, "first"); err != nil {
		t.Fatal(err)
	}
	for range out {
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}

	// Edit the project instructions between runs.
	os.WriteFile(filepath.Join(proj, "AGENTS.md"), []byte("use Go 1.27 now"), 0o644) //nolint:errcheck

	app2, err := Setup(Options{Resume: id})
	if err != nil {
		t.Fatal(err)
	}
	defer app2.Close() //nolint:errcheck

	// The effective prompt reflects the NEW value.
	sys := llm.CurrentSystemMessage(app2.Tr.Messages())
	found := false
	for _, s := range sys.Sections {
		if s.Name == "project_context" && strings.Contains(s.Value, "Go 1.27") {
			found = true
		}
	}
	if !found {
		t.Fatal("resumed prompt did not pick up the AGENTS.md edit")
	}
	// The session file carries NO system messages: the conversation is
	// all that is stored.
	for _, e := range app2.entries {
		if e.Msg != nil && e.Msg.Role == llm.RoleSystem {
			t.Fatalf("system message leaked into storage: %+v", e.Msg)
		}
	}
}
