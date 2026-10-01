package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Auto-compact off at setup parks the trigger at the disabled
// sentinel; the /config toggle re-parks it live.
func TestAutoCompactToggle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatSSE(w, "reply", 10)
	}))
	defer srv.Close()
	setupTestApp(t, srv)

	// Off via settings.json: setup resolves the disabled sentinel (it
	// wins over the explicit threshold the test app sets).
	writeSetting(t, "compactionTokens", 50000)
	writeSetting(t, "autoCompact", false)
	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck
	if app.compactTokens != -1 {
		t.Fatalf("auto-compact off: compactTokens = %d, want -1", app.compactTokens)
	}

	// Toggle on through the panel API: the explicit threshold returns.
	if err := app.SetAutoCompact(true); err != nil {
		t.Fatal(err)
	}
	if app.compactTokens != 50000 {
		t.Fatalf("auto-compact on: compactTokens = %d, want 50000 (the explicit threshold)", app.compactTokens)
	}
	// And the fresh settings agree.
	if !app.TUIConfig().AutoCompact {
		t.Fatal("toggle did not persist")
	}

	// Off again — live sentinel returns.
	if err := app.SetAutoCompact(false); err != nil {
		t.Fatal(err)
	}
	if app.compactTokens != -1 {
		t.Fatalf("re-disable: compactTokens = %d, want -1", app.compactTokens)
	}
}

// Retention: the setter persists and prunes immediately (the live
// session survives); setup prunes stale files on launch.
func TestLogRetentionPrune(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatSSE(w, "reply", 10)
	}))
	defer srv.Close()
	setupTestApp(t, srv)

	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	sessPath := app.Sess.Path

	// A stale session file next to the live one; a fresh one too.
	root := filepath.Dir(sessPath)
	stale := filepath.Join(root, "deadbeef.jsonl")
	fresh := filepath.Join(root, "cafe0000.jsonl")
	os.WriteFile(stale, []byte("{}\n"), 0o644) //nolint:errcheck
	os.WriteFile(fresh, []byte("{}\n"), 0o644) //nolint:errcheck
	old := time.Now().AddDate(0, 0, -30)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	if err := app.SetLogRetention(7); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale log survived the retention prune")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh log pruned")
	}
	if _, err := os.Stat(sessPath); err != nil {
		t.Fatal("the live session was pruned")
	}
	if c := app.TUIConfig(); c.LogRetentionDays != 7 {
		t.Fatalf("retention not persisted: %+v", c)
	}

	// Zero deletes the key (never prune).
	if err := app.SetLogRetention(0); err != nil {
		t.Fatal(err)
	}
	if c := app.TUIConfig(); c.LogRetentionDays != 0 {
		t.Fatalf("retention zero did not restore never: %+v", c)
	}
	app.Close() //nolint:errcheck

	// Setup-time pruning: launch with retention 7 kills the stale file.
	os.WriteFile(stale, []byte("{}\n"), 0o644) //nolint:errcheck
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	writeSetting(t, "logRetentionDays", 7)
	app2, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app2.Close() //nolint:errcheck
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale log survived the launch prune")
	}
}

// The REPL listing renders every panel row.
func TestConfigListText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatSSE(w, "reply", 10)
	}))
	defer srv.Close()
	setupTestApp(t, srv)
	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck
	out, _, err := app.Command("/config")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"上下文自动压缩", "日志清理周期", "Auto Memory", "Typed Memory",
		"Memory Relevance", "Memory Auto Extraction", "Rewind code", "剪贴板图片读取", "永不清理",
		"网络搜索引擎", "duckduckgo"} {
		if !strings.Contains(out, want) {
			t.Fatalf("listing missing %q:\n%s", want, out)
		}
	}
}

// Search provider: the /config setter persists into the web section,
// the snapshot reflects it, and the web tools' per-call resolver sees
// the switch live (it reloads settings, not the setup snapshot).
func TestSearchProviderToggle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatSSE(w, "reply", 10)
	}))
	defer srv.Close()
	setupTestApp(t, srv)
	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck

	// Default: duckduckgo, nothing persisted.
	if got := app.TUIConfig().SearchProvider; got != "duckduckgo" {
		t.Fatalf("default provider = %q", got)
	}
	resolve := webConfigResolver(app.Settings)
	if got := resolve().SearchProvider; got != "" {
		t.Fatalf("resolver default = %q (empty = duckduckgo)", got)
	}

	if err := app.SetSearchProvider("brave"); err != nil {
		t.Fatal(err)
	}
	if got := app.TUIConfig().SearchProvider; got != "brave" {
		t.Fatalf("persisted provider = %q", got)
	}
	// Live effect: the already-wired resolver picks the change up
	// without re-setup.
	if got := resolve().SearchProvider; got != "brave" {
		t.Fatalf("resolver did not go live: %q", got)
	}

	// Invalid values are rejected; "" deletes the key (back to default).
	if err := app.SetSearchProvider("bing"); err == nil {
		t.Fatal("unknown provider accepted")
	}
	if err := app.SetSearchProvider(""); err != nil {
		t.Fatal(err)
	}
	if got := app.TUIConfig().SearchProvider; got != "duckduckgo" {
		t.Fatalf("reset provider = %q", got)
	}
}

// writeSetting rewrites settings.json with one extra top-level key.
func writeSetting(t *testing.T, key string, value any) {
	t.Helper()
	path := filepath.Join(os.Getenv("SCODE_DIR"), "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	doc[key] = value
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, out, 0o644) //nolint:errcheck
}
