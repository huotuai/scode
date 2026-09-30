package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"scode/internal/cli"
)

// Greedy narrowing: an empty query lists everything, prefix matches
// outrank subsequence matches, non-matches drop out.
func TestMatchCommands(t *testing.T) {
	cmds := append(cli.BuiltinCommands(), cli.CommandInfo{Name: "/skill:code-review", Hint: "[args]", Desc: "review a PR"})

	if got := matchCommands("/", cmds); len(got) != len(cmds) {
		t.Fatalf("empty query = %d hits, want %d", len(got), len(cmds))
	}
	got := matchCommands("/rel", cmds)
	if len(got) != 1 || got[0].Name != "/reload" {
		t.Fatalf("/rel = %+v", got)
	}
	got = matchCommands("/re", cmds)
	// Prefix hits first (canonical order: reload, rewind, resume);
	// /skill:code-review only matches "re" as a subsequence, so it trails.
	if len(got) != 4 || got[0].Name != "/reload" || got[1].Name != "/rewind" || got[2].Name != "/resume" || got[3].Name != "/skill:code-review" {
		t.Fatalf("/re = %+v", got)
	}
	// Subsequence ("greedy") match when no prefix matches.
	got = matchCommands("/xit", cmds)
	if len(got) != 1 || got[0].Name != "/exit" {
		t.Fatalf("/xit = %+v", got)
	}
	// Prefix beats subsequence: /exit is the only prefix hit for "e";
	// everything else can only match fuzzily, so /exit leads.
	got = matchCommands("/e", cmds)
	if len(got) < 2 || got[0].Name != "/exit" {
		t.Fatalf("/e = %+v", got)
	}
	got = matchCommands("/skill:cod", cmds)
	if len(got) != 1 || got[0].Name != "/skill:code-review" {
		t.Fatalf("/skill:cod = %+v", got)
	}
	if got := matchCommands("/zzz", cmds); len(got) != 0 {
		t.Fatalf("/zzz = %+v", got)
	}
}

// setupPaletteApp builds a cli.App whose project has one skill.
func setupPaletteApp(t *testing.T) *cli.App {
	t.Helper()
	cfgDir := t.TempDir()
	proj := t.TempDir()
	settings := map[string]any{
		"defaultProvider": "openai-compat",
		"providers": map[string]any{
			"openai-compat": map[string]any{"apiKey": "k", "baseUrl": "http://127.0.0.1:1", "model": "m"},
		},
	}
	sb, _ := json.Marshal(settings)
	os.WriteFile(filepath.Join(cfgDir, "settings.json"), sb, 0o644)            //nolint:errcheck
	os.MkdirAll(filepath.Join(proj, ".scode", "skills", "code-review"), 0o755) //nolint:errcheck
	os.WriteFile(filepath.Join(proj, ".scode", "skills", "code-review", "SKILL.md"),
		[]byte("---\nname: code-review\ndescription: review a PR\n---\n\nBODY\n"), 0o644) //nolint:errcheck
	t.Setenv("SCODE_DIR", cfgDir)
	t.Chdir(proj)
	app, err := cli.Setup(cli.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() }) //nolint:errcheck
	return app
}

func typeKeys(t *testing.T, m model, keys string) model {
	t.Helper()
	for _, r := range keys {
		tm, _ := m.handleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = tm.(model)
	}
	return m
}

// The full palette flow: "/" opens everything, letters narrow greedily,
// Tab accepts into the input, Esc dismisses, an exact command submits.
func TestPaletteFlow(t *testing.T) {
	app := setupPaletteApp(t)
	m := newModel(app, make(chan any, 16))

	// "/" opens the palette with every builtin plus the skill command.
	m = typeKeys(t, m, "/")
	if !m.paletteOpen {
		t.Fatal("palette did not open on /")
	}
	want := len(cli.BuiltinCommands()) + len(cli.PromptCommandInfos()) + 2 // + skill entry + TUI-local /sandbox
	if len(m.paletteHits) != want {
		t.Fatalf("hits = %d, want %d", len(m.paletteHits), want)
	}

	// Greedy narrowing: "/rel" leaves only /reload.
	m = typeKeys(t, m, "rel")
	if len(m.paletteHits) != 1 || m.paletteHits[0].Name != "/reload" {
		t.Fatalf("hits after /rel = %+v", m.paletteHits)
	}

	// Tab accepts the highlight into the input with an args space.
	tm, _ := m.handleKey(tea.KeyPressMsg{Code: tea.KeyTab})
	m = tm.(model)
	if got := m.input.Value(); got != "/reload " {
		t.Fatalf("input after tab = %q", got)
	}
	if m.paletteOpen {
		t.Fatal("palette still open after accept")
	}

	// Back to "/", Esc dismisses without touching the text.
	m.input.SetValue("")
	m.updatePalette()
	m = typeKeys(t, m, "/")
	if !m.paletteOpen {
		t.Fatal("palette did not reopen on /")
	}
	tm, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = tm.(model)
	if m.paletteOpen {
		t.Fatal("esc did not dismiss the palette")
	}
	if got := m.input.Value(); got != "/" {
		t.Fatalf("esc changed the input: %q", got)
	}

	// Typing a full command and pressing Enter submits it (no accept):
	// /model opens the TUI's model picker.
	m = typeKeys(t, m, "model")
	tm, _ = m.handleKey(tea.KeyPressMsg{Code: '\r'})
	m = tm.(model)
	if !m.modelOpen {
		t.Fatal("/model did not open the picker")
	}
	if m.quit {
		t.Fatal("unexpected quit")
	}
}

// The palette lists /skill:name entries from the live skill set.
func TestPaletteSkillEntries(t *testing.T) {
	app := setupPaletteApp(t)
	m := newModel(app, make(chan any, 16))
	m = typeKeys(t, m, "/skill:")
	if !m.paletteOpen || len(m.paletteHits) != 1 || m.paletteHits[0].Name != "/skill:code-review" {
		t.Fatalf("hits = %+v", m.paletteHits)
	}
	// The view renders the palette without panicking.
	if v := stripANSI(m.paletteView()); !strings.Contains(v, "code-review") {
		t.Fatalf("palette view:\n%s", v)
	}
	_ = m.View()
}

// A bare "/" must show the skill command: skills lead the candidate
// list, so the row cap cannot bury them under the builtins.
func TestPaletteSkillVisibleOnSlash(t *testing.T) {
	app := setupPaletteApp(t)
	m := newModel(app, make(chan any, 16))
	m = typeKeys(t, m, "/")
	if len(m.paletteHits) != len(cli.BuiltinCommands())+len(cli.PromptCommandInfos())+2 { // + skill entry + TUI-local /sandbox
		t.Fatalf("hits = %d", len(m.paletteHits))
	}
	if m.paletteHits[0].Name != "/skill:code-review" {
		t.Fatalf("skills should lead the palette, got %+v", m.paletteHits[0])
	}
	if v := stripANSI(m.paletteView()); !strings.Contains(v, "/skill:code-review") {
		t.Fatalf("skill entry not visible on '/':\n%s", v)
	}
}

// With more candidates than paletteMaxRows, ↓ past the window scrolls
// it: the highlight stays visible and the hidden rows collapse into
// "… N above" / "… N more" indicators.
func TestPaletteScrollsToOverflow(t *testing.T) {
	app := setupPaletteApp(t)
	m := newModel(app, make(chan any, 16))
	m = typeKeys(t, m, "/")
	total := len(m.paletteHits)
	if total <= paletteMaxRows {
		t.Skipf("need overflow: %d hits", total)
	}
	// Navigate to the LAST candidate (a builtin, since skills lead).
	for i := 0; i < total-1; i++ {
		tm, _ := m.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
		m = tm.(model)
	}
	last := m.paletteHits[total-1].Name
	v := stripANSI(m.paletteView())
	if !strings.Contains(v, last) {
		t.Fatalf("last candidate %q not visible after scrolling:\n%s", last, v)
	}
	if !strings.Contains(v, "above") {
		t.Fatalf("no '… N above' indicator:\n%s", v)
	}
	// The "> /" highlight marker must sit on exactly one row — the last
	// candidate's. ("> " alone false-positives on the /resume row: its
	// "<id>" hint ends with "> " once the padding follows.)
	marked := 0
	for _, line := range strings.Split(v, "\n") {
		if strings.Contains(line, "> /") {
			marked++
			if !strings.Contains(line, last) {
				t.Fatalf("highlight on wrong row:\n%s", v)
			}
		}
	}
	if marked != 1 {
		t.Fatalf("highlight marker on %d rows, want 1:\n%s", marked, v)
	}
}
