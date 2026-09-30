package tui

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"scode/internal/mcp"
)

// The /mcp overlay flow: open, create a server through the form,
// toggle it, delete it — every step persists to mcp.json immediately.
// The command targets a missing binary: the UI flow must not depend on
// a live server (status shows connecting/reconnecting while the
// supervisor retries harmlessly in the background).
func TestMcpOverlayFlow(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	m := newModel(app, make(chan any, 16))
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(model)

	m.runCommand("/mcp")
	if !m.mcpOpen || m.mcpStage != mcpStageList {
		t.Fatal("overlay did not open on the list")
	}
	if view := plain(m.mcpView()); !strings.Contains(view, "无服务器") {
		t.Fatalf("empty list missing the hint:\n%s", view)
	}

	// n → the form; fill the name, keep stdio, type the command line,
	// cycle the policy to allow, save.
	m = press(t, m, 'n')
	if m.mcpStage != mcpStageEdit {
		t.Fatal("n did not open the form")
	}
	m = typeText(t, m, "websrv")
	m = press(t, m, tea.KeyDown) // → transport
	m = press(t, m, tea.KeyDown) // → command line
	m = typeText(t, m, "missing-scode-cmd -y example")
	m = press(t, m, tea.KeyDown)  // → policy
	m = press(t, m, tea.KeyEnter) // ask → allow
	m = press(t, m, tea.KeyDown)  // → save
	m = press(t, m, tea.KeyEnter)

	if m.mcpOpen {
		t.Fatal("save did not close the overlay")
	}
	if m.toast == nil || !strings.Contains(m.toast.text, "已保存") {
		t.Fatalf("save toast missing: %+v", m.toast)
	}
	f, err := mcp.LoadFile(filepath.Join(app.CfgDir, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, ok := f.MCPServers["websrv"]
	if !ok || cfg.Transport != "stdio" || cfg.Command != "missing-scode-cmd" ||
		len(cfg.Args) != 2 || cfg.DefaultPolicy != "allow" {
		t.Fatalf("persisted entry = %+v", cfg)
	}

	// Reopen: the list shows the server; t toggles, d d deletes.
	m.runCommand("/mcp")
	if view := plain(m.mcpView()); !strings.Contains(view, "websrv") {
		t.Fatalf("list missing the server:\n%s", view)
	}
	m = press(t, m, 't')
	f, _ = mcp.LoadFile(filepath.Join(app.CfgDir, "mcp.json"))
	if entry, ok := f.MCPServers["websrv"]; !ok || entry.IsEnabled() {
		t.Fatalf("toggle did not disable the entry: %+v", entry)
	}
	if view := plain(m.mcpView()); !strings.Contains(view, "已停用") {
		t.Fatalf("disabled state missing:\n%s", view)
	}
	m = press(t, m, 't') // back on
	m = press(t, m, 'd')
	m = press(t, m, 'd')
	f, _ = mcp.LoadFile(filepath.Join(app.CfgDir, "mcp.json"))
	if _, exists := f.MCPServers["websrv"]; exists {
		t.Fatal("delete left the entry")
	}
	m = press(t, m, tea.KeyEscape)
	if m.mcpOpen {
		t.Fatal("esc did not close the overlay")
	}
	// The main input never saw a keystroke.
	if m.input.Value() != "" {
		t.Fatalf("input polluted: %q", m.input.Value())
	}
}

// The edit screen: esc returns to the list without saving; a real edit
// goes through Restart and preserves fields the form does not cover.
func TestMcpOverlayEdit(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	path := filepath.Join(app.CfgDir, "mcp.json")
	os.WriteFile(path, []byte(`{"mcpServers":{"srv":{"transport":"stdio","command":"old","env":{"K":"V"}}}}`), 0o644) //nolint:errcheck

	m := newModel(app, make(chan any, 16))
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(model)
	m.runCommand("/mcp")
	m = press(t, m, tea.KeyEnter) // edit the first row
	if m.mcpStage != mcpStageEdit || m.mcpForm.editing != "srv" {
		t.Fatalf("enter did not open the editor: %+v", m.mcpForm)
	}
	if view := plain(m.mcpView()); strings.Contains(view, "名称") {
		t.Fatalf("edit shows the name field:\n%s", view)
	}

	// esc → back to the list without changes.
	m = press(t, m, tea.KeyEscape)
	if m.mcpStage != mcpStageList {
		t.Fatal("esc did not return to the list")
	}

	// Edit again: replace the command (ctrl+u clears the field), save,
	// and the env the form never touches survives.
	m = press(t, m, tea.KeyEnter)
	m = press(t, m, tea.KeyDown) // → command line (edit fields: transport, command, policy, save)
	tm2, _ := m.handleKey(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	m = tm2.(model)
	m = typeText(t, m, "newcmd --flag")
	m = press(t, m, tea.KeyDown) // → policy
	m = press(t, m, tea.KeyDown) // → save
	m = press(t, m, tea.KeyEnter)
	f, err := mcp.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.MCPServers["srv"]
	if cfg.Command != "newcmd" || len(cfg.Args) != 1 || cfg.Args[0] != "--flag" {
		t.Fatalf("edited command = %+v", cfg)
	}
	if cfg.Env["K"] != "V" {
		t.Fatalf("advanced field lost on edit: %+v", cfg.Env)
	}
}
