package tui

import (
	"net/http"
	"os"
	"testing"

	tea "charm.land/bubbletea/v2"

	"scode/internal/cli"
	"scode/internal/config"
)

// shift+tab in the normal input state cycles the session sandbox mode
// (workspace-write → danger-full-access → read-only → workspace-write),
// effective immediately (the per-call policy re-resolves), and each
// switch persists to the project settings.
func TestCycleSandboxShiftTab(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	m := newModel(app, make(chan any, 16))
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(model)

	if got := app.SandboxMode(); got != "workspace-write" {
		t.Fatalf("default mode = %q, want workspace-write", got)
	}
	for _, want := range []string{"danger-full-access", "read-only", "workspace-write"} {
		tm, _ := m.handleKey(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
		m = tm.(model)
		if got := app.SandboxMode(); got != want {
			t.Fatalf("after shift+tab mode = %q, want %q", got, want)
		}
	}
	// The last switch landed in .scode/settings.json.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	s, err := config.LoadProjectSettings(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if s.Sandbox == nil || s.Sandbox.Mode != "workspace-write" {
		t.Fatalf("project sandbox mode = %+v, want workspace-write", s.Sandbox)
	}
}

// A mode switched at runtime becomes the workspace default: the NEXT
// session in the same project starts in it (session replay entries
// still outrank it on resume).
func TestProjectSandboxModeSeedsNewSession(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	if err := app.SetSandboxMode("read-only"); err != nil {
		t.Fatal(err)
	}
	app2, err := cli.Setup(cli.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app2.Close() }) //nolint:errcheck
	if got := app2.SandboxMode(); got != "read-only" {
		t.Fatalf("new session mode = %q, want read-only (project default)", got)
	}
}
