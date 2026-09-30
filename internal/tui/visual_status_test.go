package tui

import (
	"net/http"
	"os"
	"testing"

	"scode/internal/cli"
)

func TestVisualDumpStatus(t *testing.T) {
	if os.Getenv("SCODE_VISUAL_DUMP") == "" {
		t.Skip("set SCODE_VISUAL_DUMP=1")
	}
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	m := newModel(app, make(chan any, 16))
	u := cli.UsageReport{ContextTokens: 185000, ContextWindow: 200000, Input: 3000, CacheRead: 9100, CostUSD: 0.5234}
	m.usage = &u
	m.running = true
	for _, mode := range []string{"read-only", "workspace-write", "danger-full-access"} {
		app.SetSandboxMode(mode) //nolint:errcheck
		m.width = 100
		t.Logf("%s raw: %q\nplain: %s\n", mode, m.statusView(), plain(m.statusView()))
	}
	app.SetSandboxMode("read-only") //nolint:errcheck
	for _, w := range []int{200, 120, 80, 60, 46, 30} {
		m.width = w
		t.Logf("w=%d:\n%s\n", w, plain(m.statusView()))
	}
}
