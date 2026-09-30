package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupTwoProviderApp writes settings with two providers so /model has
// something to switch between.
func setupTwoProviderApp(t *testing.T, srv *httptest.Server) {
	t.Helper()
	cfgDir := t.TempDir()
	proj := t.TempDir()
	settings := map[string]any{
		"defaultProvider": "openai-compat",
		"providers": map[string]any{
			"openai-compat": map[string]any{"apiKey": "k", "baseUrl": srv.URL, "model": "m1"},
			"other":         map[string]any{"apiKey": "k", "baseUrl": srv.URL, "model": "m2"},
		},
	}
	sb, _ := json.Marshal(settings)
	os.WriteFile(filepath.Join(cfgDir, "settings.json"), sb, 0o644) //nolint:errcheck
	t.Setenv("SCODE_DIR", cfgDir)
	t.Chdir(proj)
}

// /model switches: bare shows the current pair, a name argument matches
// configured pairs (exact first, substring second) and switches, an
// unknown name lists what IS configured.
func TestModelCommandSwitch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "{}")
	}))
	defer srv.Close()
	setupTwoProviderApp(t, srv)

	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck

	// Bare /model still reports the current pair.
	out, _, err := app.Command("/model")
	if err != nil || !strings.Contains(out, "openai-compat / m1") {
		t.Fatalf("/model = %q, %v", out, err)
	}

	// Exact provider name switches (and persists as the default).
	out, _, err = app.Command("/model other")
	if err != nil || !strings.Contains(out, "other / m2") || !strings.Contains(out, "saved as default") {
		t.Fatalf("/model other = %q, %v", out, err)
	}
	if p, m := app.CurrentModel(); p != "other" || m != "m2" {
		t.Fatalf("model = %s / %s, want other / m2", p, m)
	}

	// Substring on the label switches back.
	out, _, err = app.Command("/model compat")
	if err != nil || !strings.Contains(out, "openai-compat / m1") {
		t.Fatalf("/model compat = %q, %v", out, err)
	}
	if p, _ := app.CurrentModel(); p != "openai-compat" {
		t.Fatalf("model = %s, want openai-compat", p)
	}

	// Unknown name lists the configured pairs instead of switching.
	out, _, err = app.Command("/model nope")
	if err != nil || !strings.Contains(out, "no configured model matches") || !strings.Contains(out, "other / m2") {
		t.Fatalf("/model nope = %q, %v", out, err)
	}
	if p, _ := app.CurrentModel(); p != "openai-compat" {
		t.Fatalf("failed match changed the model: %s", p)
	}
}

// Persistence: a /model switch survives BOTH restart paths — the
// settings-level default (fresh sessions) and the session log (resume
// replay), and thinking entries replay the same way.
func TestModelCommandPersists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "{}")
	}))
	defer srv.Close()
	setupTwoProviderApp(t, srv)

	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	id := app.Sess.Header().ID
	if _, _, err := app.Command("/model other"); err != nil {
		t.Fatal(err)
	}
	if err := app.SetThinkingLevel("high"); err != nil {
		t.Fatal(err)
	}
	// The TUI picker's commit additionally records the effort as the
	// settings default (mirroring that flow).
	if err := app.PersistDefaultThinking("high"); err != nil {
		t.Fatal(err)
	}
	app.Close() //nolint:errcheck

	// Fresh session: resolves the persisted default.
	fresh, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close() //nolint:errcheck
	if p, m := fresh.CurrentModel(); p != "other" || m != "m2" {
		t.Fatalf("fresh session model = %s / %s, want other / m2", p, m)
	}
	if lv := fresh.CurrentThinkingLevel(); lv != "high" {
		t.Fatalf("fresh session thinking = %q, want high", lv)
	}
	fresh.Close() //nolint:errcheck

	// Resume: the session log's model/thinking entries win over the
	// (identical here) defaults.
	resumed, err := Setup(Options{Resume: id})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close() //nolint:errcheck
	if p, m := resumed.CurrentModel(); p != "other" || m != "m2" {
		t.Fatalf("resumed model = %s / %s, want other / m2", p, m)
	}
	if lv := resumed.CurrentThinkingLevel(); lv != "high" {
		t.Fatalf("resumed thinking = %q, want high", lv)
	}
}
