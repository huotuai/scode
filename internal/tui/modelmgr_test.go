package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"scode/internal/config"
)

// typeText runs printable characters through the model (the key-stage
// text input reads KeyPressMsg.Text).
func typeText(t *testing.T, m model, s string) model {
	t.Helper()
	for _, r := range s {
		tm, _ := m.handleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = tm.(model)
	}
	return m
}

// /models walks the preset catalog in four stages — vendor → key →
// model → confirm — and the final Enter writes the profile into
// settings, switches the session onto it, and makes it visible to
// /model. A vendor carries several models; each configures separately
// with the saved key offered for reuse.
func TestModelManagerFlow(t *testing.T) {
	app := setupTwoModelApp(t, nil)
	m := newModel(app, make(chan any, 16))
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	// Stage one: the preset vendor list (from the embedded catalog).
	m.runCommand("/models")
	if !m.modelsOpen || m.modelsStage != modelsStageVendor {
		t.Fatal("manager did not open on the vendor list")
	}
	view := plain(m.modelsView())
	if !strings.Contains(view, "Anthropic") || !strings.Contains(view, "DeepSeek") {
		t.Fatalf("preset vendors missing:\n%s", view)
	}

	// Esc on the vendor list cancels outright.
	m = press(t, m, tea.KeyEscape)
	if m.modelsOpen {
		t.Fatal("Esc on the vendor list did not close the manager")
	}

	// Full flow on the first vendor (Anthropic): Enter → key stage.
	m.runCommand("/models")
	m = press(t, m, tea.KeyEnter)
	if m.modelsStage != modelsStageKey {
		t.Fatal("Enter did not advance to the key stage")
	}
	// Typing fills the key buffer (masked in the view).
	m = typeText(t, m, "sk-test-12345")
	if m.keyBuf != "sk-test-12345" {
		t.Fatalf("key buffer = %q", m.keyBuf)
	}
	if view := plain(m.modelsView()); !strings.Contains(view, "sk-…2345") || strings.Contains(view, "sk-test-12345") {
		t.Fatalf("key not masked in the view:\n%s", view)
	}
	// Backspace edits; retype the missing character.
	m = press(t, m, tea.KeyBackspace)
	if m.keyBuf != "sk-test-1234" {
		t.Fatalf("backspace: key buffer = %q", m.keyBuf)
	}
	m = typeText(t, m, "5")

	// Enter → model stage: the vendor's preset models with parameters.
	m = press(t, m, tea.KeyEnter)
	if m.modelsStage != modelsStageModel {
		t.Fatal("Enter did not advance to the model stage")
	}
	view = plain(m.modelsView())
	if !strings.Contains(view, "Claude Sonnet 4.5") || !strings.Contains(view, "上下文 200K") || !strings.Contains(view, "推理") {
		t.Fatalf("model stage missing preset parameters:\n%s", view)
	}

	// Esc steps back to the key stage, then forward again.
	m = press(t, m, tea.KeyEscape)
	if m.modelsStage != modelsStageKey {
		t.Fatal("Esc did not step back to the key stage")
	}
	m = press(t, m, tea.KeyEnter)

	// ↓ onto claude-sonnet-4-5 (opus is first), Enter → confirm stage:
	// the summary plus the reasoning-strength rows, preset default marked.
	m = press(t, m, tea.KeyDown)
	m = press(t, m, tea.KeyEnter)
	if m.modelsStage != modelsStageConfirm {
		t.Fatal("Enter did not advance to the confirm stage")
	}
	view = plain(m.modelsView())
	for _, want := range []string{"厂商", "Anthropic", "claude-sonnet-4-5", "anthropic", "预设", "providers.anthropic:claude-sonnet-4-5"} {
		if !strings.Contains(view, want) {
			t.Fatalf("confirm stage missing %q:\n%s", want, view)
		}
	}

	// ↑ from the preset default (medium) onto low, Enter commits.
	m = press(t, m, tea.KeyUp)
	m = press(t, m, tea.KeyEnter)
	if m.modelsOpen {
		t.Fatal("manager still open after the final Enter")
	}

	// The profile landed in settings.json (key, protocol, preset params)…
	s, err := config.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	pc, ok := s.Providers["anthropic:claude-sonnet-4-5"]
	if !ok {
		t.Fatalf("profile not written: %v", s.Providers)
	}
	if pc.APIKey != "sk-test-12345" || pc.Protocol != "anthropic" || pc.Model != "claude-sonnet-4-5" ||
		pc.ContextWindow != 200000 || pc.MaxTokens != 64000 || !pc.Reasoning {
		t.Fatalf("profile = %+v", pc)
	}
	// …the session switched onto it with the chosen strength…
	if p, mid := app.CurrentModel(); p != "anthropic:claude-sonnet-4-5" || mid != "claude-sonnet-4-5" {
		t.Fatalf("model = %s / %s", p, mid)
	}
	if lv := app.CurrentThinkingLevel(); lv != "low" {
		t.Fatalf("thinking = %q, want low", lv)
	}
	// …it became the default…
	if s.DefaultProvider != "anthropic:claude-sonnet-4-5" || s.DefaultModel != "claude-sonnet-4-5" || s.DefaultThinkingLevel != "low" {
		t.Fatalf("defaults = %s / %s / %q", s.DefaultProvider, s.DefaultModel, s.DefaultThinkingLevel)
	}
	// …and /model's data source sees it.
	seen := false
	for _, c := range app.ConfiguredModels() {
		if c.Provider == "anthropic:claude-sonnet-4-5" && c.Model == "claude-sonnet-4-5" && c.Reasoning {
			seen = true
		}
	}
	if !seen {
		t.Fatal("configured model not visible to /model")
	}
	last := plain(m.blocks[len(m.blocks)-1].rendered)
	if !strings.Contains(last, "已配置") || !strings.Contains(last, "已存为默认") {
		t.Fatalf("confirmation note missing: %q", last)
	}

	// A second model of the SAME vendor configures separately, reusing
	// the saved key (the key stage offers it; Enter keeps it).
	m.runCommand("/models")
	if view := plain(m.modelsView()); !strings.Contains(view, "已配置") {
		t.Fatalf("configured vendor not marked:\n%s", view)
	}
	m = press(t, m, tea.KeyEnter) // vendor: anthropic (still highlighted)
	if view := plain(m.modelsView()); !strings.Contains(view, "已保存 Key") {
		t.Fatalf("saved key not offered for reuse:\n%s", view)
	}
	m = press(t, m, tea.KeyEnter) // keep the saved key → model stage
	// The current pair (sonnet) is preselected; ↓ onto haiku.
	m = press(t, m, tea.KeyDown)
	m = press(t, m, tea.KeyEnter) // confirm stage
	m = press(t, m, tea.KeyEnter) // commit (haiku's preset default: off)
	s, err = config.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	pc, ok = s.Providers["anthropic:claude-haiku-4-5"]
	if !ok {
		t.Fatal("second profile not written")
	}
	if pc.APIKey != "sk-test-12345" {
		t.Fatalf("saved key not reused: %q", pc.APIKey)
	}
	if len(s.Providers) != 4 { // openai-compat + other + two anthropic profiles
		t.Fatalf("providers = %v", s.Providers)
	}
	if p, _ := app.CurrentModel(); p != "anthropic:claude-haiku-4-5" {
		t.Fatalf("model = %s, want the haiku profile", p)
	}
}
