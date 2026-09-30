package tui

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"scode/internal/agent"
	"scode/internal/cli"
	"scode/internal/llm"
)

// sgrRe strips SGR sequences (the package's stripANSI helper stops at
// "[" and leaves the parameters behind).
var sgrRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// sgrBgRe matches basic (40–47) background SGR codes.
var sgrBgRe = regexp.MustCompile(`\x1b\[4[0-7]m`)

func plain(s string) string { return sgrRe.ReplaceAllString(s, "") }

// renderThinking: blank-line runs collapse to single newlines and the
// visible tail caps at thinkingMaxLines behind a header.
func TestRenderThinkingCompactAndCrop(t *testing.T) {
	m := newTestModel() // width 80
	var paras []string
	for i := range 30 {
		paras = append(paras, fmt.Sprintf("paragraph %d of thinking", i))
	}
	raw := strings.Join(paras, "\n\n\n") // paragraph gaps arrive as blank runs
	out := plain(m.renderThinking(raw))
	lines := strings.Split(out, "\n")
	// header + 10 visible lines
	if len(lines) != thinkingMaxLines+1 {
		t.Fatalf("cropped thinking = %d lines, want %d:\n%s", len(lines), thinkingMaxLines+1, out)
	}
	if !strings.Contains(lines[0], "ctrl+o") {
		t.Fatalf("header missing expand hint: %q", lines[0])
	}
	// Cropped to the TAIL: early paragraphs are gone, the last is visible.
	if strings.Contains(out, "paragraph 0") {
		t.Fatal("cropped thinking still shows the first paragraph")
	}
	if !strings.Contains(out, "paragraph 29") {
		t.Fatal("cropped thinking lost the last paragraph")
	}
	// Compacted: no blank lines inside the visible body.
	for _, l := range lines[1:] {
		if strings.TrimSpace(l) == "" {
			t.Fatalf("blank line survived compaction: %q", out)
		}
	}
}

// Short thinking renders without a header; expanded mode shows all
// lines with no crop.
func TestRenderThinkingShortAndExpanded(t *testing.T) {
	m := newTestModel()
	short := "line one\n\nline two"
	out := plain(m.renderThinking(short))
	lines := strings.Split(out, "\n")
	// Guttered: gray dot on the first line, continuation indented to the
	// text column; blank runs collapse.
	if len(lines) != 2 || lines[0] != " ● line one" || lines[1] != "   line two" {
		t.Fatalf("guttered short thinking = %#v", lines)
	}
	if strings.Contains(out, "ctrl+o") {
		t.Fatalf("short thinking got a header: %q", out)
	}

	var paras []string
	for i := range 30 {
		paras = append(paras, fmt.Sprintf("paragraph %d", i))
	}
	m.expandThinking = true
	out = plain(m.renderThinking(strings.Join(paras, "\n\n")))
	if !strings.Contains(out, "paragraph 0") || !strings.Contains(out, "paragraph 29") {
		t.Fatalf("expanded thinking lost content:\n%s", out)
	}
	if strings.Contains(out, "ctrl+o") {
		t.Fatalf("expanded thinking still shows the crop header: %q", out)
	}
}

// ctrl+o toggles expansion of committed thinking blocks (and the live
// thinking buffer keeps the cropped rendering regardless).
func TestCtrlOTogglesThinkingExpansion(t *testing.T) {
	m := newTestModel()
	var paras []string
	for i := range 20 {
		paras = append(paras, fmt.Sprintf("thought %d", i))
	}
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventThinkingDelta, Delta: strings.Join(paras, "\n")}})
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventThinkingEnd}})
	if len(m.blocks) != 1 || !m.blocks[0].isThink { // the thinking block only — spacing leads items
		t.Fatalf("thinking block = %#v", m.blocks)
	}
	if strings.Contains(plain(m.blocks[0].rendered), "thought 0") {
		t.Fatal("committed thinking not cropped")
	}

	tm, _ := m.handleKey(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	m = tm.(model)
	if !m.expandThinking {
		t.Fatal("ctrl+o did not set expandThinking")
	}
	if !strings.Contains(plain(m.blocks[0].rendered), "thought 0") {
		t.Fatal("ctrl+o did not expand the committed block")
	}

	tm, _ = m.handleKey(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	m = tm.(model)
	if m.expandThinking {
		t.Fatal("second ctrl+o did not collapse")
	}
	if strings.Contains(plain(m.blocks[0].rendered), "thought 0") {
		t.Fatal("collapse did not re-crop the block")
	}
}

// Streaming deltas accumulate into live WITHOUT touching the viewport;
// the flush tick syncs them (~30fps re-layout instead of per-delta).
func TestStreamingThrottledByFlushTick(t *testing.T) {
	m := newTestModel()
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextDelta, Delta: "hello"}})
	if !m.dirty {
		t.Fatal("delta did not mark the model dirty")
	}
	if strings.Contains(m.vp.GetContent(), "hello") {
		t.Fatal("delta reached the viewport before the flush tick")
	}
	// The tick fires once: no re-arm while one is in flight.
	if cmd := m.maybeFlushTick(); cmd == nil {
		t.Fatal("maybeFlushTick did not arm")
	}
	if !m.flushScheduled {
		t.Fatal("flushScheduled not set")
	}
	if cmd := m.maybeFlushTick(); cmd != nil {
		t.Fatal("maybeFlushTick armed a second tick")
	}
	tm, _ := m.Update(flushTickMsg{})
	m = tm.(model)
	if m.flushScheduled || m.dirty {
		t.Fatal("tick did not clear the flush state")
	}
	if !strings.Contains(m.vp.GetContent(), "hello") {
		t.Fatal("tick did not sync the viewport")
	}
}

// The input box: rounded border, plain top rule, exactly m.width
// cells per line; a label appears only while a plan review is pending.
func TestInputBoxView(t *testing.T) {
	m := newTestModel()
	// Match newModel's composer: dynamic height, one line when empty.
	m.input.DynamicHeight = true
	m.input.MinHeight = 1
	m.input.MaxHeight = 8
	m.input.ShowLineNumbers = false
	m.input.Prompt = "❯ "
	m.input.SetWidth(m.width - 2)
	m.input.Focus()
	box := m.inputBoxView()
	lines := strings.Split(box, "\n")
	if len(lines) != 3 { // top rule + 1 textarea line + bottom border
		t.Fatalf("input box = %d lines, want 3:\n%s", len(lines), box)
	}
	top := plain(lines[0])
	if !strings.HasPrefix(top, "╭") || !strings.HasSuffix(top, "╮") {
		t.Fatalf("top rule missing rounded corners: %q", top)
	}
	if strings.Contains(top, "❯") {
		t.Fatalf("top rule should have no label by default: %q", top)
	}
	for i, l := range lines {
		if w := lipgloss.Width(l); w != m.width {
			t.Fatalf("line %d width = %d, want %d: %q", i, w, m.width, plain(l))
		}
	}
	bottom := plain(lines[2])
	if !strings.HasPrefix(bottom, "╰") || !strings.HasSuffix(bottom, "╯") {
		t.Fatalf("bottom border malformed: %q", bottom)
	}

	// Plan review switches the label.
	m.pending = &approvalRequest{kind: "plan", answer: make(chan string, 1)}
	if got := plain(m.inputBoxView()); !strings.Contains(got, "计划反馈") {
		t.Fatalf("plan label missing:\n%s", got)
	}
	m.pending = nil

	// Degenerate width: no chrome, no panic.
	m.width = 3
	_ = m.inputBoxView()
}

// resize is cheap on the typing path: an unchanged width must NOT
// re-wrap the transcript (rewrap is the expensive operation).
func TestResizeSkipsRecompositionOnSameWidth(t *testing.T) {
	m := newTestModel()
	m.contentWidth = m.width // simulate a prior resize
	m.appendBlock("sentinel")
	before := m.vp.GetContent()
	m.resize()
	if after := m.vp.GetContent(); after != before {
		t.Fatalf("resize re-composed the transcript:\nbefore=%q\nafter=%q", before, after)
	}
	// Width change DOES re-compose (thinking blocks re-wrap).
	m.width = 120
	m.resize()
	if m.contentWidth != 120 {
		t.Fatal("width change not tracked")
	}
}

// The view always fills the window height exactly (box chrome accounted
// for in the layout math): the transcript pads with blank lines when
// short, so the composer and status bar stay pinned to the bottom of
// the window instead of riding up under the content.
func TestViewHeightMatchesWindow(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	m := newModel(app, make(chan any, 16))
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	v := m.view()
	if got := lipgloss.Height(string(v.Content)); got != m.height {
		t.Fatalf("short-content view height = %d, want exactly %d:\n%s", got, m.height, v.Content)
	}
	// Fill the transcript past the viewport: still exact.
	for i := range 100 {
		m.appendBlock(fmt.Sprintf("line %d", i))
	}
	v = m.view()
	if got := lipgloss.Height(string(v.Content)); got != m.height {
		t.Fatalf("full view height = %d, want %d:\n%s", got, m.height, v.Content)
	}
}

// statusView: the sandbox state anchors the left edge and the working
// directory the right one; the model carries its reasoning effort, and
// the usage fragment shows context occupancy plus the average cache hit
// rate. Narrow terminals shrink the directory, then drop segments, and
// the bar never wraps.
func TestStatusViewLayout(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	m := newModel(app, make(chan any, 16))
	u := cli.UsageReport{
		ContextTokens: 12000, ContextWindow: 200000,
		Input: 3000, CacheRead: 9000,
		CostUSD: 0.0123,
	}
	m.usage = &u

	m.width = 200
	bar := plain(m.statusView())
	for _, want := range []string{
		"可写", // default policy: workspace-write
		"m · 默认",
		"ctx 12k/200k",
		"缓存 75%",
	} {
		if !strings.Contains(bar, want) {
			t.Errorf("status bar missing %q:\n%s", want, bar)
		}
	}
	// Left-to-right order: sandbox mode first, model after it.
	if i, j := strings.Index(bar, "可写"), strings.Index(bar, "m · 默认"); i < 0 || j < i {
		t.Fatalf("sandbox mode does not lead the bar:\n%s", bar)
	}
	// The working directory right-aligns: last non-space content, bar
	// padded to the full width. Compare against the home-collapsed form
	// (the temp dir sits under the user's home).
	if want := collapseHome(app.CWD); !strings.HasSuffix(strings.TrimRight(bar, " "), want) {
		t.Fatalf("cwd does not end the bar (want tail %q):\n%s", want, bar)
	}
	if got := lipgloss.Width(bar); got != 200 {
		t.Fatalf("bar width = %d, want 200:\n%s", got, bar)
	}
	// No background anywhere on the bar: nested styles reset SGR, so an
	// outer background would break after the first segment and show as a
	// stray color block behind the leading text.
	if raw := m.statusView(); strings.Contains(raw, "48;") || sgrBgRe.MatchString(raw) {
		t.Fatalf("status bar paints a background:\n%q", raw)
	}

	// Narrow bar: still exactly one line, nothing wraps.
	m.width = 46
	bar = plain(m.statusView())
	if strings.Contains(bar, "\n") || lipgloss.Width(bar) > 46 {
		t.Fatalf("narrow bar wrapped:\n%s", bar)
	}

	// Degenerate widths never panic and stay single-line.
	for _, w := range []int{0, 1, 8, 20} {
		m.width = w
		bar = plain(m.statusView())
		if strings.Contains(bar, "\n") {
			t.Fatalf("width %d bar wrapped:\n%s", w, bar)
		}
	}
}

// Reasoning-effort label follows the desktop's THINKING_LABELS vocabulary.
func TestThinkingLabel(t *testing.T) {
	for lv, want := range map[string]string{
		"": "默认", "off": "关闭", "low": "低", "medium": "中", "high": "高",
	} {
		if got := thinkingLabel(lv); got != want {
			t.Errorf("thinkingLabel(%q) = %q, want %q", lv, got, want)
		}
	}
}

func TestTruncateLeft(t *testing.T) {
	if got := truncateLeft("abcdefghij", 20); got != "abcdefghij" {
		t.Errorf("truncateLeft(fit) = %q", got)
	}
	if got := truncateLeft("abcdefghij", 4); got != "…hij" {
		t.Errorf("truncateLeft = %q, want …hij", got)
	}
}

func TestCollapseHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	if got := collapseHome(filepath.Join(home, "proj", "x")); got != filepath.Join("~", "proj", "x") {
		t.Errorf("collapseHome = %q", got)
	}
	if got := collapseHome(`E:\elsewhere`); got != `E:\elsewhere` {
		t.Errorf("collapseHome(non-home) = %q", got)
	}
}

// truncate and briefArgs cut at rune boundaries: byte-based slicing
// split CJK runes and leaked invalid UTF-8 into the transcript.
func TestTruncateAndBriefArgsCJK(t *testing.T) {
	cjk := strings.Repeat("中", 40) // 3 bytes per rune
	got := truncate(cjk, 5)
	if want := strings.Repeat("中", 5) + "…"; got != want {
		t.Fatalf("truncate CJK = %q, want %q", got, want)
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncate produced invalid UTF-8")
	}
	ba := briefArgs([]byte(`{"command":"` + strings.Repeat("跑", 60) + `"}`))
	if !utf8.ValidString(ba) || !strings.HasSuffix(ba, "…") {
		t.Fatalf("briefArgs CJK = %q", ba)
	}
}

// The startup banner: logotype + info column (model, cwd, session) +
// hints, every line inside the wrap width even with pathological
// model ids and deep paths.
func TestWelcomeBanner(t *testing.T) {
	b := welcomeBanner("anthropic", "claude-sonnet-4", `E:\work\proj`, "20260929-153000-0001")
	lines := strings.Split(b, "\n")
	if len(lines) != 8 { // 5 art rows + blank + 2 hint lines
		t.Fatalf("banner = %d lines:\n%s", len(lines), b)
	}
	plain := plainText(b)
	if !strings.Contains(plain, "anthropic") || !strings.Contains(plain, "claude-sonnet-4") ||
		!strings.Contains(plain, `E:\work\proj`) || !strings.Contains(plain, "20260929") ||
		!strings.Contains(plain, "steer") {
		t.Fatalf("banner info missing:\n%s", plain)
	}
	checkWidth := func(banner string) {
		t.Helper()
		for i, l := range strings.Split(banner, "\n") {
			if w := ansi.StringWidth(plainText(l)); w > 74 {
				t.Fatalf("banner line %d width %d > 74:\n%s", i, w, banner)
			}
		}
	}
	checkWidth(b)
	// Pathologically long ids/paths truncate instead of wrapping — the
	// path keeps its tail (the nearest directories identify the
	// project).
	long := welcomeBanner("openai-compat", "gpt-"+strings.Repeat("x", 60), strings.Repeat(`X:\dir\`, 20), strings.Repeat("s", 60))
	checkWidth(long)
	if plainLong := plainText(long); !strings.Contains(plainLong, "…") {
		t.Fatalf("long inputs produced no truncation mark:\n%s", plainLong)
	}
}
