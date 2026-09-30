package tui

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"scode/internal/i18n"
	"scode/internal/plantrack"
)

var (
	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	thinkStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Italic(true)
	toolStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	userStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Bold(true) // 用户消息：橙色（✨ 角标自带金色 emoji 呈现）
	warnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)

	// Transcript status dots (column 0 of every guttered row).
	dotOK      = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))  // tool succeeded
	dotFail    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))  // tool failed
	dotSandbox = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))  // sandbox-blocked
	dotBody    = lipgloss.NewStyle().Foreground(lipgloss.Color("15")) // assistant text

	// Work-status tail line (yellow, star icon).
	spinStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))

	// Mouse-selection highlight (reverse video reads on light and dark
	// themes alike, and survives the styled lines it is spliced into).
	selStyle = lipgloss.NewStyle().Reverse(true)

	// Transient top-right toast (copy confirmations and failures).
	toastStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Background(lipgloss.Color("236")).Padding(0, 1)
)

// spinFrames is the work-status animation cycle. Terminals cannot truly
// rotate a glyph, so the "rotation" phase cycles the six-pointed star
// through visibly different star variants (✶ → ✻ teardrop asterisk →
// ✽ heavy burst), then the breathing phase grows ✶→✷→✸ and shrinks
// back — then the rotation repeats. Every codepoint here is
// text-presentation: emoji codepoints (✳ U+2733, ✴ U+2734…) render as
// green emoji on Windows Terminal, ignoring the yellow style.
var spinFrames = []string{"✶", "✻", "✽", "✻", "✶", "✷", "✸", "✷"}

// bannerArt is the SCODE logotype (figlet "standard" face, hand-set —
// pure ASCII so every terminal and font renders it identically).
var bannerArt = []string{
	" ____    ____    ___    ____   _____ ",
	"/ ___|  / ___|  / _ \\  |  _ \\ | ____|",
	"\\___ \\ | |     | | | | | | | ||  _|  ",
	" ___) || |___  | |_| | | |_| || |___ ",
	"|____/  \\____|  \\___/  |____/ |_____|",
}

// bannerLineStyles gradient the logotype violet → cyan → magenta →
// orange, landing on the app's accent (208) — the banner reads as one
// piece with the ✨ user-echo color.
var bannerLineStyles = []lipgloss.Style{
	lipgloss.NewStyle().Foreground(lipgloss.Color("141")),
	lipgloss.NewStyle().Foreground(lipgloss.Color("111")),
	lipgloss.NewStyle().Foreground(lipgloss.Color("75")),
	lipgloss.NewStyle().Foreground(lipgloss.Color("207")),
	lipgloss.NewStyle().Foreground(lipgloss.Color("208")),
}

// bannerTruncate cuts s to at most w CELLS, rune-safe, with an
// ellipsis — model ids can be arbitrarily long and must never push a
// banner line past the wrap width.
func bannerTruncate(s string, w int) string {
	if ansi.StringWidth(s) <= w {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		rw := ansi.StringWidth(string(r))
		if ansi.StringWidth(b.String())+rw > w-1 {
			break
		}
		b.WriteRune(r)
	}
	return b.String() + "…"
}

// welcomeBanner renders the startup banner: the logotype with an info
// column (model, cwd, session) vertically centered beside it, key
// hints below. Every line stays ≤ 74 cells so the default 80-column
// layout never wraps it.
func welcomeBanner(prov, modelID, cwd, session string) string {
	artW := 0
	for _, l := range bannerArt {
		if w := ansi.StringWidth(l); w > artW {
			artW = w
		}
	}
	infoCap := 74 - artW - 4 // every banner line stays inside the wrap budget
	if infoCap < 10 {
		infoCap = 10
	}
	info := []string{
		userStyle.Render(bannerTruncate("✨ "+prov+" · "+modelID, infoCap)),
		// The cwd keeps its TAIL when over budget — the nearest
		// directories are what identifies the project.
		dimStyle.Render("📁 " + truncateLeft(collapseHome(cwd), infoCap-3)),
		dimStyle.Render(bannerTruncate(i18n.Tf("tui.render.session", session), infoCap)),
	}
	var b strings.Builder
	for i, l := range bannerArt {
		b.WriteString(bannerLineStyles[i].Render(l))
		if i >= 1 && i <= 3 { // info column beside art rows 2-4
			b.WriteString(strings.Repeat(" ", artW-ansi.StringWidth(l)+4) + info[i-1])
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(dimStyle.Render(i18n.T("tui.render.hintLine1")) + "\n")
	b.WriteString(dimStyle.Render(i18n.T("tui.render.hintLine2")))
	return b.String()
}

// workPhase is the run phase the tail line reflects.
type workPhase int

const (
	phaseIdle       workPhase = iota
	phaseRequesting           // LLM request in flight, nothing streamed yet
	phaseWorking              // model is responding (streaming or running tools)
)

// label reports the tail line's English label for the phase.
func (s workPhase) label() string {
	switch s {
	case phaseRequesting:
		return "Requesting..."
	case phaseWorking:
		return "Working..."
	}
	return ""
}

// Input box chrome (Claude Code style): a rounded border around the
// textarea with a labelled top rule rendered manually. The box style
// carries only the left/right/bottom sides; the top line (with its
// embedded label) is built by inputBoxView.
var (
	inputBorderColor = lipgloss.Color("8")
	inputBorderStyle = lipgloss.NewStyle().Foreground(inputBorderColor)
	inputBoxStyle    = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderTop(false).
				BorderForeground(inputBorderColor)
)

// The transcript gutter: a dot in column 1 (one cell off the left edge,
// a single space from its text), text from column 3 — the same column
// the composer's text starts at (border cell + "> " prompt), so session
// content and typed input align.
const gutterPad = "   " // leading space + dot cell + trailing space

// gutterText is the column where guttered text begins (also the wrap
// margin gutter-aware renderers subtract).
const gutterText = 3

// gutterView prefixes the first line with a colored dot and indents
// continuation lines to the text column.
func gutterView(dot lipgloss.Style, s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if i == 0 {
			lines[i] = " " + dot.Render("●") + " " + l
		} else {
			lines[i] = gutterPad + l
		}
	}
	return strings.Join(lines, "\n")
}

// noteLine renders a transcript status note — a sandbox/model/thinking
// switch echo or its "can't switch while running" guard. The body is
// prefixed with a ❗ marker: the glyph's two cells plus the trailing
// space are exactly gutterText cells, so the note text lines up with
// ordinary guttered content instead of starting flush left.
func noteLine(s string) string {
	return dimStyle.Render("❗ " + s)
}

// capitalize upper-cases the first rune (tool names read as "Bash",
// "Read", …).
func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// toolLine renders one tool-call row: status dot, capitalized name, and
// the one-line argument summary dimmed.
func toolLine(dot lipgloss.Style, name, args string) string {
	return " " + dot.Render("●") + " " + capitalize(name) + "  " + dimStyle.Render(args)
}

// overlayView frames popup content (palette, file picker, sandbox
// picker) in the composer's visual language: the same rounded border and
// labelled top rule as the input box. Callers indent rows so their text
// lands on the composer's text column.
func (m *model) overlayView(title, body string) string {
	return m.overlayBox("8", title, body)
}

// overlayBox is overlayView with a custom border color: warn-yellow
// frames decision popups (plan approval), input-dim the command
// surfaces. Content rows carry a 2-space indent so their text lands
// under the title (the rule reads "╭─ title", so the title text sits
// two cells in from the corner), and the frame pads one blank line
// above and below the body — the terminal has no real line leading,
// and that air is what keeps popup rows from touching.
func (m *model) overlayBox(borderColor, title, body string) string {
	w := m.width
	if w < 10 {
		return body // degenerate widths: no chrome
	}
	bs := lipgloss.NewStyle().Foreground(lipgloss.Color(borderColor))
	lead := "─ " + title + " "
	inner := w - 2 // cells between the corner glyphs
	dashes := inner - lipgloss.Width(lead)
	if dashes < 1 {
		lead, dashes = "─", inner-1
	}
	top := bs.Render("╭" + lead + strings.Repeat("─", dashes) + "╮")
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderTop(false).
		BorderForeground(lipgloss.Color(borderColor)).
		Padding(1, 0)
	return top + "\n" + box.Width(w).Render(body)
}

// overlayList joins popup rows with a blank line between them: list
// items read as separated entries instead of a dense block. Rows are
// the caller's logical rows (items, then hint lines); the highlight
// markers ("> ") and 2-space indents are part of each row.
func overlayList(rows []string) string {
	return strings.Join(rows, "\n\n")
}

// thinkingMaxLines caps the visible tail of a thinking block; the
// rest collapses behind a header line (ctrl+o toggles the full text).
const thinkingMaxLines = 10

// thinkingTailBytes bounds live-thinking rendering per frame: the crop
// keeps only the newest wrapped lines, so a long stream can wrap a
// bounded suffix instead of the entire accumulated buffer. The rendered
// tail is identical either way (wrapping is per raw line, so the
// wrapped suffix of a suffix is the suffix of the wrapped whole).
const thinkingTailBytes = 12 * 1024

var thinkingBlankRun = regexp.MustCompile(`\n{2,}`)

// renderThinking renders a committed thinking block in full fidelity.
func (m *model) renderThinking(raw string) string {
	return m.renderThinkingCapped(raw, 0)
}

// renderThinkingCapped compacts blank-line runs (thinking paragraphs
// arrive with double newlines, which read as huge gaps in the
// transcript), wraps to the transcript width, and crops to the last
// thinkingMaxLines lines unless the user expanded thinking blocks. The
// crop means a streaming block visually scrolls inside a fixed 10-line
// region instead of flooding the transcript. With capBytes > 0 the
// transform runs on only the newest suffix — bounded per-frame cost for
// the live view; the exact line count in the header then reflects the
// processed suffix. The whole block sits in the gutter under a gray
// dot (matching the gray thinking text).
func (m *model) renderThinkingCapped(raw string, capBytes int) string {
	src := raw
	if capBytes > 0 && len(raw) > capBytes {
		cut := len(raw) - capBytes
		// Start at a line boundary inside the cap window: a '\n' scan
		// never splits a UTF-8 rune (a raw byte cut can). The search is
		// bounded to the window so the scan stays O(cap), not O(raw).
		lo := max(0, cut-4096)
		if i := strings.LastIndexByte(raw[lo:cut], '\n'); i >= 0 {
			cut = lo + i + 1
		}
		src = strings.TrimLeft(raw[cut:], "\r\n")
	}
	text := strings.ReplaceAll(src, "\r\n", "\n")
	text = thinkingBlankRun.ReplaceAllString(strings.TrimRight(text, "\n"), "\n")
	w := m.width - gutterText // reserve the gutter so wrapped lines fit
	if w < 20 {
		w = 20
	}
	lines := strings.Split(lipgloss.Wrap(text, w, ""), "\n")
	hidden := 0
	if !m.expandThinking && len(lines) > thinkingMaxLines {
		hidden = len(lines) - thinkingMaxLines
		lines = lines[hidden:]
	}
	body := thinkStyle.Render(strings.Join(lines, "\n"))
	if hidden > 0 {
		header := dimStyle.Render(i18n.Tf("tui.render.thinkingFold", hidden+thinkingMaxLines))
		return gutterView(dimStyle, header+"\n"+body)
	}
	return gutterView(dimStyle, body)
}

// inputBoxView renders the textarea inside a rounded border whose top
// rule embeds a label, separating the composer from the transcript.
func (m *model) inputBoxView() string {
	w := m.width
	if w < 6 {
		return m.input.View() // degenerate widths: drop the chrome
	}
	// Plain top rule by default; only a pending plan review gets a label.
	label := ""
	if m.pending != nil && m.pending.kind == "plan" {
		label = i18n.T("tui.render.planFeedbackLabel")
	}
	lead := "─" + label
	inner := w - 2 // cells between the corner glyphs
	dashes := inner - lipgloss.Width(lead)
	if dashes < 0 {
		lead, dashes = "─", inner-1
	}
	top := inputBorderStyle.Render("╭" + lead + strings.Repeat("─", dashes) + "╮")
	// lipgloss Width INCLUDES the border cells, so the body totals w.
	body := inputBoxStyle.Width(w).Render(m.input.View())
	return top + "\n" + body
}

// approvalView renders the pending decision box above the input. Tool
// and sandbox asks get a bordered box with button actions (the arrows
// move the focus, Enter activates) in the plan review's visual
// language; a plan review keeps its own scrollable window (see
// planApprovalView).
func (m *model) approvalView() string {
	req := m.pending
	if req.kind == "plan" {
		return m.planApprovalView()
	}
	if m.width < 16 {
		return warnStyle.Render("── "+req.title+" ──") + "\n" + req.body + "\n" + req.hint
	}
	inner := max(20, m.width-4)
	body := lipgloss.NewStyle().Width(inner).Render(strings.TrimSpace(req.body))
	rows := make([]string, 0, 8)
	for _, l := range strings.Split(body, "\n") {
		rows = append(rows, "  "+l) // text under the title column
	}
	rows = append(rows, "", m.approvalButtonsRow(),
		dimStyle.Render(i18n.Tf("tui.render.approvalHint", req.hint)))
	return m.overlayBox("3", req.title, strings.Join(rows, "\n"))
}

// approvalButtonsRow renders the tool/sandbox ask's action buttons:
// the focused one inverts (orange fill), the rest dim outlines — the
// same visual language as the plan-review buttons.
func (m *model) approvalButtonsRow() string {
	btns := approvalButtons(m.pending.kind)
	cells := make([]string, len(btns))
	for i, b := range btns {
		cells[i] = buttonCell(b.Label, i == m.approvalBtn)
	}
	return "  " + strings.Join(cells, "  ") // under the title column
}

// planButtons are the plan-review actions in display order; Enter sends
// the focused one (Send back turns the typed input — or a generic note
// when empty — into revision feedback). A function (not a var) so the
// display language is read at render time.
func planButtons() []string {
	return []string{i18n.T("tui.render.planApprove"), i18n.T("tui.render.planRevise"), i18n.T("tui.render.planReject")}
}

// planApprovalView renders the plan review: the plan body scrolls inside
// a height-capped window (a long plan never swallows the screen), the
// actions are buttons navigated with ←/→, and typed text still doubles
// as revision feedback on Enter.
func (m *model) planApprovalView() string {
	req := m.pending
	w := m.width
	if w < 16 {
		return warnStyle.Render("── "+i18n.T("tui.render.planTitle")+" ──") + "\n" + req.body
	}
	lines, budget := m.planWindow()
	scroll := min(m.planScroll, max(0, len(lines)-budget))
	if scroll < 0 {
		scroll = 0
	}
	end := min(len(lines), scroll+budget)
	rows := make([]string, 0, budget+4)
	for _, l := range lines[scroll:end] {
		rows = append(rows, "  "+l) // text under the title column
	}
	if len(lines) > budget {
		rows = append(rows, dimStyle.Render(i18n.Tf("tui.render.planLines", scroll+1, end, len(lines))))
	}
	rows = append(rows, "", m.planButtonsRow(), dimStyle.Render(i18n.T("tui.render.planHint")))
	return m.overlayBox("3", i18n.T("tui.render.planTitle"), strings.Join(rows, "\n"))
}

// buttonCell renders one action button: focused inverts (orange
// fill), unfocused a dim outline. Fg/bg styles only — the box has no
// background to break.
func buttonCell(label string, focused bool) string {
	if focused {
		return lipgloss.NewStyle().
			Background(lipgloss.Color("208")).
			Foreground(lipgloss.Color("16")).
			Bold(true).
			Render(" " + label + " ")
	}
	return dimStyle.Render("[ " + label + " ]")
}

// planButtonsRow renders the action buttons: the focused one inverts
// (orange fill), the rest dim outlines.
func (m *model) planButtonsRow() string {
	btns := planButtons()
	cells := make([]string, len(btns))
	for i, label := range btns {
		cells[i] = buttonCell(label, i == m.planBtn)
	}
	return "  " + strings.Join(cells, "  ") // under the title column
}

// planWindow wraps the plan body to the box width and reports the
// visible-line budget: about half the screen minus the chrome (the
// frame's two padding rows included), so the transcript keeps room of
// its own.
func (m *model) planWindow() (lines []string, budget int) {
	inner := max(20, m.width-4)
	lines = strings.Split(lipgloss.Wrap(strings.TrimSpace(m.pending.body), inner, ""), "\n")
	budget = max(3, min(40, m.height/2-6))
	return lines, budget
}

// Status-bar accents: fg-only styles, no background (see statusView).
var (
	statusModelStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	statusEffortStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	statusCacheStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	statusPlanStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	statusRunStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	statusSep         = dimStyle.Render(" │ ")
)

// statusFg colors one segment on the status bar.
func statusFg(c string) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(c))
}

// statusSeg is one status-bar segment: the rendered text in visual order
// plus its drop priority. Narrow terminals shed segments by ascending
// priority; 0 is mandatory and never drops.
type statusSeg struct {
	text string
	drop int
}

// statusView renders the bottom bar. The sandbox mode anchors the left
// edge and the working directory the right one; between them run the
// model (reasoning effort appended), context occupancy, average cache
// hit rate and the plan/run markers. On narrow terminals the directory
// first shrinks from the left, then optional segments drop, before the
// bar ever wraps.
//
// The line renders WITHOUT a background: nested per-segment styles each
// end in an SGR reset, which would kill an outer background after the
// first segment and leave a stray color block behind the leading text.
func (m *model) statusView() string {
	cwd := collapseHome(m.app.CWD)
	segs := m.statusSegments()
	w := m.width
	if w <= 0 {
		return " " + joinStatusSegs(segs) + "  " + cwd
	}
	for cwd != "" {
		left := " " + joinStatusSegs(segs)
		lw, cw := lipgloss.Width(left), lipgloss.Width(cwd)
		if lw+2+cw <= w {
			return left + strings.Repeat(" ", w-lw-cw) + dimStyle.Render(cwd)
		}
		// Still too wide: shrink the directory to what's left (keeping
		// its tail — the nearest directories matter most), then drop the
		// lowest-value segments, before giving up the directory.
		if room := w - lw - 2; room >= 8 {
			cwd = truncateLeft(cwd, room)
			continue
		}
		if !dropStatusSeg(&segs) {
			break
		}
	}
	// The mandatory segments alone can still overflow a very narrow bar;
	// clip ANSI-aware so the line never wraps.
	return ansi.Truncate(" "+joinStatusSegs(segs), w, "")
}

// joinStatusSegs concatenates the surviving segments with the separator.
func joinStatusSegs(segs []statusSeg) string {
	parts := make([]string, len(segs))
	for i, s := range segs {
		parts[i] = s.text
	}
	return strings.Join(parts, statusSep)
}

// dropStatusSeg removes the lowest-priority optional segment; reports
// whether anything was left to drop.
func dropStatusSeg(segs *[]statusSeg) bool {
	low := -1
	for i, s := range *segs {
		if s.drop > 0 && (low < 0 || s.drop < (*segs)[low].drop) {
			low = i
		}
	}
	if low < 0 {
		return false
	}
	*segs = append((*segs)[:low], (*segs)[low+1:]...)
	return true
}

// statusSegments builds the bar's content, visual order left → right:
// sandbox mode, model · effort, ctx (tiered by occupancy), plan/run
// markers, average cache hit rate — e.g.
//
//	只读 │ m · 默认 │ ctx 185k/200k │ 缓存 75% │ ● running    ~/proj
func (m *model) statusSegments() []statusSeg {
	segs := make([]statusSeg, 0, 6)

	// Sandbox mode word leads the bar, colored by confinement (see
	// sandboxBadge — the picker shares the vocabulary).
	sbWord, sbColor := sandboxBadge(m.app.SandboxMode())
	segs = append(segs, statusSeg{text: statusFg(sbColor).Render(sbWord)})

	// Model id with the reasoning effort appended (always shown, "默认"
	// follows the provider default — mirrors the desktop picker).
	_, modelID := m.app.CurrentModel()
	model := statusModelStyle.Render(modelID) +
		dimStyle.Render(" · ") +
		statusEffortStyle.Render(thinkingLabel(m.app.CurrentThinkingLevel()))
	segs = append(segs, statusSeg{text: model})

	// Context occupancy against the model window; the tier colors match
	// the desktop gauge (yellow ≥60%, red ≥85%).
	if m.usage != nil {
		ctxText := "ctx " + humanTokens(m.usage.ContextTokens)
		var ctxStyle lipgloss.Style
		if m.usage.ContextWindow > 0 {
			ctxText += "/" + humanTokens(int64(m.usage.ContextWindow))
			switch pct := 100 * m.usage.ContextTokens / int64(m.usage.ContextWindow); {
			case pct >= 85:
				ctxStyle = statusFg("1")
			case pct >= 60:
				ctxStyle = statusFg("3")
			}
		}
		segs = append(segs, statusSeg{text: ctxStyle.Render(ctxText)})

		// Average cache hit rate: cache reads over all prompt-side reads.
		cacheText := i18n.T("tui.render.cacheNone")
		if reads := m.usage.Input + m.usage.CacheRead; reads > 0 {
			cacheText = i18n.Tf("tui.render.cache", (100*m.usage.CacheRead+reads/2)/reads)
		}
		segs = append(segs, statusSeg{text: statusCacheStyle.Render(cacheText), drop: 2})

	}

	// Plan progress (the update_plan checklist) rides the bar so the
	// current step is visible even between checklist blocks.
	if plan, ok := m.app.PlanState(); ok {
		done := 0
		for _, it := range plan.Items {
			if it.Status == plantrack.Completed {
				done++
			}
		}
		segs = append(segs, statusSeg{
			text: statusPlanStyle.Render(i18n.Tf("tui.render.planProgress", done, len(plan.Items))),
			drop: 2,
		})
	}
	if m.app.PlanMode() {
		segs = append(segs, statusSeg{text: statusPlanStyle.Render("[plan]"), drop: 3})
	}
	if m.running {
		segs = append(segs, statusSeg{text: statusRunStyle.Render("● running"), drop: 4})
	}
	return segs
}

// thinkingLabel maps a reasoning effort to its bar label ("" follows the
// provider default; mirrors the desktop's THINKING_LABELS).
func thinkingLabel(lv string) string {
	switch lv {
	case "off":
		return i18n.T("tui.render.effortOff")
	case "low":
		return i18n.T("tui.render.effortLow")
	case "medium":
		return i18n.T("tui.render.effortMedium")
	case "high":
		return i18n.T("tui.render.effortHigh")
	}
	return i18n.T("tui.render.effortDefault")
}

// collapseHome shortens a home-relative path to ~/… (prefix match is
// case-insensitive: Windows paths drift between cases).
func collapseHome(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || p == "" {
		return p
	}
	if len(p) >= len(home) && strings.EqualFold(p[:len(home)], home) {
		return "~" + p[len(home):]
	}
	return p
}

// truncateLeft keeps the tail of s within w cells, prefixed with an
// ellipsis when cut — for paths the nearest directories matter most.
func truncateLeft(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	r := []rune(s)
	for i := range r {
		if lipgloss.Width(string(r[i:])) <= w-1 {
			return "…" + string(r[i:])
		}
	}
	return "…"
}

// briefArgs renders tool arguments on one line for status output.
func briefArgs(raw []byte) string {
	const max = 72
	return truncate(string(raw), max)
}

// truncate shortens s to n runes with an ellipsis — byte-based cutting
// would split a CJK rune and leak invalid UTF-8 into the transcript.
func truncate(s string, n int) string {
	if n < 1 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1_000_000), ".0") + "M"
	case n >= 1_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1_000), ".0") + "k"
	default:
		return fmt.Sprintf("%d", n)
	}
}
