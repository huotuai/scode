package tui

import (
	"fmt"
	"strings"

	"scode/internal/cli"
	"scode/internal/i18n"
)

// Command completion palette: typing "/" opens the slash-command list
// (builtins + prompt commands), typing "$" opens the skill list, and
// each further keystroke narrows it — prefix matches rank above
// subsequence ("greedy") matches. ↑/↓ navigate (the window scrolls to
// follow the highlight), Tab accepts into the input, Enter accepts
// unless the typed text already IS a full command (then it submits),
// Esc dismisses until the text changes.

// paletteMaxRows caps the visible candidates; an overflow line counts
// the rest.
const paletteMaxRows = 8

// matchCommands ranks cmds against the typed query (with or without the
// leading "/" or "$" marker): prefix matches first (canonical order),
// then subsequence matches. An empty query returns everything.
func matchCommands(query string, cmds []cli.CommandInfo) []cli.CommandInfo {
	q := strings.ToLower(strings.TrimLeft(query, "/$"))
	if q == "" {
		return cmds
	}
	var prefix, fuzzy []cli.CommandInfo
	for _, c := range cmds {
		name := strings.ToLower(strings.TrimLeft(c.Name, "/$"))
		if strings.HasPrefix(name, q) {
			prefix = append(prefix, c)
		} else if subsequence(name, q) {
			fuzzy = append(fuzzy, c)
		}
	}
	return append(prefix, fuzzy...)
}

// subsequence reports whether q's bytes appear in s in order.
func subsequence(s, q string) bool {
	i := 0
	for j := 0; j < len(s) && i < len(q); j++ {
		if s[j] == q[i] {
			i++
		}
	}
	return i == len(q)
}

// updatePalette recomputes the palette state from the current input:
// open only while the FIRST token is a "/..." or "$..." fragment (a
// space or newline means the command part is done) and the fragment is
// not the one Esc just dismissed (paletteEsc — update() re-runs this on
// every blink/focus message, so a bare text check would resurrect the
// list half a second after Esc; editing the text re-arms it).
// Candidates refresh every keystroke, so a skill hot reload shows up
// immediately.
func (m *model) updatePalette() {
	text := m.input.Value()
	lead := byte(0)
	if text != "" {
		lead = text[0]
	}
	if m.pending != nil || (lead != '/' && lead != '$') || strings.ContainsAny(text, " \n") {
		m.paletteEsc = "" // left the command form: re-arm
		m.paletteOpen = false
		m.paletteHits = nil
		m.paletteIdx = 0
		return
	}
	if text == m.paletteEsc {
		// Esc-dismissed; stay closed until the text changes.
		m.paletteOpen = false
		m.paletteHits = nil
		m.paletteIdx = 0
		return
	}
	var cmds []cli.CommandInfo
	if lead == '$' {
		// "$..." is the skill namespace: only $name entries apply.
		if m.app != nil {
			cmds = append(cmds, m.app.SkillCommands()...)
		}
	} else {
		// "/..." lists the slash commands; skills live under "$" now.
		cmds = append(cmds, cli.BuiltinCommands()...)
		// Prompt commands (/commit, ...) execute through the prompt
		// path, not App.Command — but they are first-class palette
		// entries.
		cmds = append(cmds, cli.PromptCommandInfos()...)
		// TUI-local commands (the overlay lives here, not in cli).
		cmds = append(cmds,
			cli.CommandInfo{Name: "/sandbox", Hint: "", Desc: i18n.T("tui.palette.sandboxDesc")},
		)
	}
	m.paletteHits = matchCommands(text, cmds)
	m.paletteOpen = len(m.paletteHits) > 0
	if m.paletteIdx >= len(m.paletteHits) {
		m.paletteIdx = 0
	}
}

// paletteExactMatch reports whether the typed text IS one of the
// candidates in full (Enter should submit it, not complete it).
func (m *model) paletteExactMatch() bool {
	text := m.input.Value()
	for _, h := range m.paletteHits {
		if h.Name == text {
			return true
		}
	}
	return false
}

// acceptPalette fills the highlighted candidate into the input (with a
// trailing space for args) and closes the palette.
func (m *model) acceptPalette() {
	if m.paletteIdx >= len(m.paletteHits) {
		return
	}
	m.input.SetValue(m.paletteHits[m.paletteIdx].Name + " ")
	m.input.CursorEnd()
	m.updatePalette() // the trailing space closes the palette
	m.resize()
}

// paletteView renders the candidate list above the input as a window
// that scrolls to keep the highlight visible: the highlighted row gets
// the ❯ marker, descriptions dim and truncate to the remaining width,
// and hidden candidates collapse into "… N above" / "… N more" lines.
func (m *model) paletteView() string {
	hits := m.paletteHits
	before, after := 0, 0
	if len(hits) > paletteMaxRows {
		start := m.paletteIdx - paletteMaxRows + 1 // scroll just enough
		if start < 0 {
			start = 0
		}
		if max := len(hits) - paletteMaxRows; start > max {
			start = max
		}
		before = start
		after = len(hits) - start - paletteMaxRows
		hits = hits[start : start+paletteMaxRows]
	}
	hl := m.paletteIdx - before // highlight index within the window
	labelW := 0
	labelWidth := func(h cli.CommandInfo) int {
		n := len([]rune(h.Name))
		if h.Hint != "" {
			n += 1 + len([]rune(h.Hint))
		}
		return n
	}
	for _, h := range hits {
		if n := labelWidth(h); n > labelW {
			labelW = n
		}
	}
	var rows []string
	if before > 0 {
		rows = append(rows, dimStyle.Render(fmt.Sprintf("  … %d above", before)))
	}
	for i, h := range hits {
		label := h.Name
		if h.Hint != "" {
			label += " " + dimStyle.Render(h.Hint)
		}
		pad := strings.Repeat(" ", labelW-labelWidth(h))
		desc := truncateRunes(h.Desc, m.width-labelW-8)
		row := label + pad + "  " + dimStyle.Render(desc)
		if i == hl {
			rows = append(rows, userStyle.Render("> ")+row)
		} else {
			rows = append(rows, "  "+row)
		}
	}
	if after > 0 {
		rows = append(rows, dimStyle.Render(fmt.Sprintf("  … %d more", after)))
	}
	return m.overlayView(i18n.T("tui.palette.title"), strings.Join(rows, "\n"))
}

// truncateRunes shortens s to n runes with an ellipsis (byte-based
// truncation could split a CJK rune).
func truncateRunes(s string, n int) string {
	if n < 1 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}
