package tui

import (
	"net/http"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"scode/internal/agent"
	"scode/internal/llm"
)

// The tool dot lifecycle: start renders a gray-dotted capitalized row;
// the settled call recolors the SAME row in place (green / red /
// sandbox-orange) and the scroller sees the spliced line — no duplicate
// row, no stale text.
func TestToolDotLifecycle(t *testing.T) {
	run := func(res agent.ToolResult) model {
		m := newTestModel()
		call := llm.Block{Kind: llm.BlockToolCall, ID: "c1", Name: "bash", Arguments: []byte(`{"command":"ls"}`)}
		m.renderEvent(agent.Event{Type: agent.EvToolStart, Call: &call})
		if got := plainText(m.blocks[0].rendered); !strings.HasPrefix(got, " ● Bash") {
			t.Fatalf("tool row = %q", got)
		}
		m.renderEvent(agent.Event{Type: agent.EvToolEnd, Call: &call, Result: &res})
		return m
	}

	m := run(agent.ToolResult{})
	if !strings.Contains(m.blocks[0].rendered, "\x1b[32m") { // green
		t.Fatalf("success not green: %q", m.blocks[0].rendered)
	}
	if got := plainText(m.vp.GetContent()); !strings.Contains(got, "Bash") || strings.Contains(got, "\x1b") {
		t.Fatalf("scroller not spliced: %q", got)
	}

	m = run(agent.ToolResult{IsError: true})
	if !strings.Contains(m.blocks[0].rendered, "\x1b[31m") { // red
		t.Fatalf("failure not red: %q", m.blocks[0].rendered)
	}

	m = run(agent.ToolResult{IsError: true, Content: []llm.Block{
		llm.TextBlock("[sandbox: file access denied under read-only mode]"),
	}})
	if !strings.Contains(m.blocks[0].rendered, "\x1b[33m") { // orange
		t.Fatalf("sandbox denial not orange: %q", m.blocks[0].rendered)
	}
	if got := plainText(m.blocks[1].rendered); !strings.Contains(got, "sandbox") {
		t.Fatalf("denial excerpt missing: %q", got)
	}
}

// Alignment: the composer's text and the transcript's guttered text
// start at the same column (border + "> " prompt == dot + two spaces).
func TestComposerAlignment(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	m := newModel(app, make(chan any, 16))
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	// One assistant body line: "●  hello…" → text at column 3 (count in
	// runes: ● and the box border are multi-byte).
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextDelta, Delta: "hello"}})
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextEnd}})
	var body []rune
	for _, b := range m.blocks { // the last block is the spacer
		if strings.Contains(b.rendered, "hello") {
			body = []rune(plainText(b.rendered))
		}
	}
	if i := runeIndex(body, "hello"); i != 3 {
		t.Fatalf("body text starts at column %d, want 3: %q", i, string(body))
	}

	// Composer: border(1) + "> " prompt(2) → text at column 3.
	m.input.SetValue("word")
	box := plainText(m.inputBoxView())
	lines := strings.Split(box, "\n")
	var inputLine string
	for _, l := range lines {
		if strings.Contains(l, "word") {
			inputLine = l
		}
	}
	if inputLine == "" {
		t.Fatalf("input line missing from box:\n%s", box)
	}
	if i := runeIndex([]rune(inputLine), "word"); i != 3 {
		t.Fatalf("input text starts at column %d, want 3: %q", i, inputLine)
	}
}

// runeIndex finds sub's first rune-column in rs (-1 when absent).
func runeIndex(rs []rune, sub string) int {
	subRunes := []rune(sub)
	for i := 0; i+len(subRunes) <= len(rs); i++ {
		match := true
		for j, r := range subRunes {
			if rs[i+j] != r {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// The work-status tail line: one blank line below the content, yellow
// star + "Requesting..." while the LLM call is in flight, "Working..."
// once the model responds, gone when the run ends; the spin tick steps
// the animation frames.
func TestWorkStatusTail(t *testing.T) {
	app := setupImageApp(t)
	m := newModel(app, make(chan any, 16))

	m.phase = phaseRequesting
	m.syncTail()
	content := plainText(m.vp.GetContent())
	if !strings.Contains(content, "Requesting...") || !strings.Contains(content, spinFrames[0]) {
		t.Fatalf("requesting tail missing: %q", content)
	}
	if m.vp.live[len(m.vp.live)-2] != "" {
		t.Fatalf("status line not separated by a blank line: %#v", m.vp.live)
	}

	// First response-side event flips the label.
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextDelta, Delta: "hi"}})
	if m.phase != phaseWorking {
		t.Fatalf("phase = %v, want working", m.phase)
	}
	m.syncTail()
	if content := plainText(m.vp.GetContent()); !strings.Contains(content, "Working...") {
		t.Fatalf("working tail missing: %q", content)
	}

	// The animation advances one frame per spin tick.
	tm, cmd := m.Update(spinTickMsg{})
	m = tm.(model)
	if cmd == nil {
		t.Fatal("spin tick did not re-arm while running")
	}
	if m.spinFrame != 1 {
		t.Fatalf("spinFrame = %d, want 1", m.spinFrame)
	}
	if content := plainText(m.vp.GetContent()); !strings.Contains(content, spinFrames[1]) {
		t.Fatalf("frame did not advance: %q", content)
	}

	// Run end clears the tail line.
	m.phase = phaseIdle
	m.syncTail()
	if content := plainText(m.vp.GetContent()); strings.Contains(content, "...") {
		t.Fatalf("status line survived the run: %q", content)
	}
}

// Animation frames must be text-presentation codepoints: emoji
// codepoints (U+2733, U+2734, U+2721…) render as green emoji on Windows
// Terminal, ignoring the yellow spin style (the "green square" bug).
func TestSpinFramesNotEmoji(t *testing.T) {
	emoji := map[rune]bool{
		0x2721: true, // ✡ Star of David
		0x2733: true, // ✳ eight-spoked asterisk
		0x2734: true, // ✴ eight-pointed star
		0x2744: true, // ❄ snowflake
		0x2747: true, // ❇ sparkle
		0x2763: true, // ❣ heart exclamation
	}
	for i, f := range spinFrames {
		for _, r := range f {
			if emoji[r] || r >= 0x1F000 {
				t.Fatalf("frame %d %q uses emoji-presentation rune %U", i, f, r)
			}
		}
	}
	if len(spinFrames) < 6 {
		t.Fatalf("animation too short: %d frames", len(spinFrames))
	}
}

// The echoed user prompt: orange (256-color 208) with the gold ✨
// marker; the emoji's two cells + one space land the text at the gutter
// column.
func TestUserEchoStyle(t *testing.T) {
	app := setupImageApp(t)
	m := newModel(app, make(chan any, 256))
	m.input.SetValue("hello world")
	tm, _ := m.submit()
	m = tm.(model)
	var line string
	for _, b := range m.blocks {
		if strings.Contains(b.rendered, "hello world") {
			line = b.rendered
		}
	}
	if line == "" {
		t.Fatal("user echo line missing")
	}
	if !strings.Contains(line, "38;5;208") {
		t.Fatalf("user line not orange: %q", line)
	}
	body := []rune(plainText(line))
	if body[0] != '✨' {
		t.Fatalf("marker = %q, want ✨", string(body[0]))
	}
	if i := runeIndex(body, "hello world"); i != 2 {
		t.Fatalf("user text rune index = %d, want 2: %q", i, string(body))
	}
	// Display column: the emoji renders two cells wide, so ✨+space spans
	// columns 0-2 and the text starts at column 3 like every guttered row.
	if w := lipgloss.Width(string(body[:2])); w != 3 {
		t.Fatalf("marker spans %d display cells, want 3: %q", w, string(body[:2]))
	}
}
