package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"scode/internal/cli"
	"scode/internal/config"
)

// setupTwoModelApp configures two providers so the picker has a real
// choice to switch to.
func setupTwoModelApp(t *testing.T, handler http.HandlerFunc) *cli.App {
	t.Helper()
	srv := httptest.NewServer(handler)
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
	app, err := cli.Setup(cli.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() }) //nolint:errcheck
	return app
}

// press runs keystrokes through the model (test shorthand).
func press(t *testing.T, m model, codes ...rune) model {
	t.Helper()
	for _, c := range codes {
		tm, _ := m.handleKey(tea.KeyPressMsg{Code: c})
		m = tm.(model)
	}
	return m
}

// /model runs two stages in one command: the model list, then the
// reasoning effort; the final Enter commits both together.
func TestModelPickerCommand(t *testing.T) {
	app := setupTwoModelApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	m := newModel(app, make(chan any, 16))
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	m.runCommand("/model")
	if !m.modelOpen || m.modelStage != modelStageList {
		t.Fatal("picker did not open on the model list")
	}
	view := plain(m.modelPickerView())
	if !strings.Contains(view, "openai-compat / m1") || !strings.Contains(view, "other / m2") {
		t.Fatalf("configured pairs missing from the picker:\n%s", view)
	}
	if !strings.Contains(view, "当前") {
		t.Fatalf("current model not marked:\n%s", view)
	}

	// ↓ lands on the other pair; Enter advances to the effort stage with
	// the pending model as the header and the current effort marked.
	m = press(t, m, tea.KeyDown)
	m = press(t, m, tea.KeyEnter)
	if !m.modelOpen || m.modelStage != modelStageEffort {
		t.Fatal("Enter did not advance to the effort stage")
	}
	view = plain(m.modelPickerView())
	if !strings.Contains(view, "other / m2") || !strings.Contains(view, "推理强度") || !strings.Contains(view, "默认") {
		t.Fatalf("effort stage missing chrome:\n%s", view)
	}
	// Esc steps back to the model list (nothing applied yet).
	m = press(t, m, tea.KeyEscape)
	if !m.modelOpen || m.modelStage != modelStageList {
		t.Fatal("Esc did not step back to the model list")
	}
	if p, _ := app.CurrentModel(); p != "openai-compat" {
		t.Fatalf("step-back applied the model anyway: %s", p)
	}

	// Full flow (highlight still on "other" after the step-back):
	// Enter (effort stage) + ↓×4 (默认→关闭→低→中→高) + Enter commits
	// BOTH switches in one note.
	m = press(t, m, tea.KeyEnter)
	m = press(t, m, tea.KeyDown, tea.KeyDown, tea.KeyDown, tea.KeyDown)
	m = press(t, m, tea.KeyEnter)
	if m.modelOpen {
		t.Fatal("picker still open after the final Enter")
	}
	if p, mid := app.CurrentModel(); p != "other" || mid != "m2" {
		t.Fatalf("model = %s / %s, want other / m2", p, mid)
	}
	if lv := app.CurrentThinkingLevel(); lv != "high" {
		t.Fatalf("thinking = %q, want high", lv)
	}
	last := plain(m.blocks[len(m.blocks)-1].rendered)
	if !strings.Contains(last, "other / m2") || !strings.Contains(last, "高") {
		t.Fatalf("combined note missing parts: %q", last)
	}
	if !strings.Contains(last, "已存为默认") {
		t.Fatalf("save note missing: %q", last)
	}
	// The status bar follows both switches.
	if bar := plain(m.statusView()); !strings.Contains(bar, "m2 · 高") {
		t.Fatalf("status bar did not follow the switch:\n%s", bar)
	}

	// The choice persisted to settings.json as the defaults…
	s, err := config.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if s.DefaultProvider != "other" || s.DefaultModel != "m2" || s.DefaultThinkingLevel != "high" {
		t.Fatalf("settings defaults = %s / %s / %q", s.DefaultProvider, s.DefaultModel, s.DefaultThinkingLevel)
	}
	// …so a fresh session (new launch) starts from it.
	app.Close() //nolint:errcheck
	app2, err := cli.Setup(cli.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app2.Close() //nolint:errcheck
	if p, mid := app2.CurrentModel(); p != "other" || mid != "m2" {
		t.Fatalf("fresh session model = %s / %s", p, mid)
	}
	if lv := app2.CurrentThinkingLevel(); lv != "high" {
		t.Fatalf("fresh session thinking = %q, want high", lv)
	}

	// A fully unchanged pick (same model, same effort) closes with only
	// the save confirmation.
	n := len(m.blocks)
	m.runCommand("/model")
	m = press(t, m, tea.KeyEnter) // current pair already highlighted
	m = press(t, m, tea.KeyEnter) // current effort (高) already highlighted
	if m.modelOpen || len(m.blocks) != n+1 || plain(m.blocks[len(m.blocks)-1].rendered) != "❗ 已存为默认" {
		t.Fatalf("unchanged pick should only confirm the save (blocks %d → %d)", n, len(m.blocks))
	}

	// Esc on the model list cancels outright.
	m.runCommand("/model")
	m = press(t, m, tea.KeyEscape)
	if m.modelOpen {
		t.Fatal("picker still open after Esc on the list")
	}
	if p, _ := app.CurrentModel(); p != "other" {
		t.Fatalf("Esc changed the model: %s", p)
	}

	// "/model <name>" bypasses the picker and switches directly.
	m.runCommand("/model openai-compat")
	if p, mid := app.CurrentModel(); p != "openai-compat" || mid != "m1" {
		t.Fatalf("arg switch failed: %s / %s", p, mid)
	}
}
