package tui

import (
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Markdown pipe-table rendering for assistant text. The streaming tail
// shows the raw pipe lines (the incremental wrap cache owns that path);
// once a text block flushes, its tables snap into column-aligned form —
// and width changes re-render from the block's kept raw text, so a
// resize re-aligns instead of hard-cutting rows.
//
// Scope is deliberately GFM's pipe table only: a header line containing
// '|', a delimiter row of dashes/colons, then body rows. Fenced code
// blocks are NOT scanned (a "table" inside ``` stays literal). Inline
// markdown inside cells (**, `) is left as literal text like the rest
// of the transcript.

var (
	// tableHeadStyle bolds the header row; tableRuleStyle dims the
	// delimiter line. Foreground stays untouched so the rows inherit
	// the transcript's default text color.
	tableHeadStyle = lipgloss.NewStyle().Bold(true)
	tableRuleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// tableAlign is a column's horizontal alignment, from the delimiter
// row's colons (GFM: :--- left, ---: right, :---: center).
type tableAlign int

const (
	alignLeft tableAlign = iota
	alignRight
	alignCenter
)

// sepCellRe validates one delimiter cell: dashes with optional
// leading/trailing colons.
var sepCellRe = regexp.MustCompile(`^:?-+:?$`)

// mdTable is one parsed pipe table (columns normalized to the widest
// row; missing cells pad with "").
type mdTable struct {
	header []string
	align  []tableAlign
	rows   [][]string
}

// renderTablesInText scans text line by line and replaces every pipe
// table with its aligned rendering (rows capped at width cells; a
// table that cannot fit even shrunk renders as the original lines).
// Non-table lines pass through unchanged.
func renderTablesInText(text string, width int) []string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	inCode := false
	for i := 0; i < len(lines); {
		if isFence(lines[i]) {
			inCode = !inCode
			out = append(out, lines[i])
			i++
			continue
		}
		if !inCode {
			if tbl, next, ok := parseTable(lines, i); ok {
				if rendered := renderTable(tbl, width); rendered != nil {
					out = append(out, rendered...)
				} else {
					out = append(out, lines[i:next]...) // too narrow: keep the raw lines
				}
				i = next
				continue
			}
		}
		out = append(out, lines[i])
		i++
	}
	return out
}

// isFence reports a fenced-code boundary (``` or ~~~, info string
// allowed) — the toggle that keeps code samples literal.
func isFence(line string) bool {
	s := strings.TrimSpace(line)
	return strings.HasPrefix(s, "```") || strings.HasPrefix(s, "~~~")
}

// parseTable reads a table starting at lines[i]: a header containing
// '|', a delimiter row, then body rows (lines containing '|'). Column
// counts are normalized to the widest row — models emit ragged tables
// often enough that strict GFM equality would reject real output.
func parseTable(lines []string, i int) (tbl mdTable, next int, ok bool) {
	if i+1 >= len(lines) || !strings.Contains(lines[i], "|") {
		return mdTable{}, i, false
	}
	align, ok := parseDelimiter(lines[i+1])
	if !ok {
		return mdTable{}, i, false
	}
	tbl.header = splitCells(lines[i])
	tbl.align = align
	n := max(len(tbl.header), len(align))
	next = i + 2
	for next < len(lines) && strings.Contains(lines[next], "|") && strings.TrimSpace(lines[next]) != "" {
		cells := splitCells(lines[next])
		n = max(n, len(cells))
		tbl.rows = append(tbl.rows, cells)
		next++
	}
	// Normalize: every row exactly n cells, the align row included.
	tbl.header = normCells(tbl.header, n)
	tbl.align = normAlign(align, n)
	for j := range tbl.rows {
		tbl.rows[j] = normCells(tbl.rows[j], n)
	}
	return tbl, next, true
}

// parseDelimiter validates a delimiter row and derives column
// alignment. At least two cells or one explicit pipe — a bare "---"
// line is a horizontal rule, not a one-column table delimiter.
func parseDelimiter(line string) ([]tableAlign, bool) {
	s := strings.TrimSpace(line)
	if s == "" || !strings.Contains(s, "|") {
		return nil, false
	}
	cells := splitCells(s)
	aligns := make([]tableAlign, len(cells))
	for i, c := range cells {
		c = strings.ReplaceAll(c, " ", "")
		if !sepCellRe.MatchString(c) {
			return nil, false
		}
		switch {
		case strings.HasPrefix(c, ":") && strings.HasSuffix(c, ":") && len(c) > 2:
			aligns[i] = alignCenter
		case strings.HasSuffix(c, ":"):
			aligns[i] = alignRight
		default:
			aligns[i] = alignLeft
		}
	}
	return aligns, true
}

// splitCells splits a pipe-table line into trimmed cells. Outer pipes
// are optional; \| escapes a literal pipe inside a cell.
func splitCells(line string) []string {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")
	var cells []string
	var cur strings.Builder
	for k := 0; k < len(s); k++ {
		switch c := s[k]; {
		case c == '\\' && k+1 < len(s) && s[k+1] == '|':
			cur.WriteByte('|')
			k++
		case c == '|':
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(cells, strings.TrimSpace(cur.String()))
}

func normCells(cells []string, n int) []string {
	for len(cells) < n {
		cells = append(cells, "")
	}
	return cells[:n]
}

func normAlign(align []tableAlign, n int) []tableAlign {
	for len(align) < n {
		align = append(align, alignLeft)
	}
	return align[:n]
}

// renderTable lays out tbl column-aligned within width cells. Column
// widths are display widths (CJK/ANSI-aware); when the natural table
// exceeds width, the widest column shrinks one cell at a time and its
// cells truncate with an ellipsis. nil when even the minimal layout
// overflows (the caller falls back to the raw lines).
func renderTable(tbl mdTable, width int) []string {
	n := len(tbl.header)
	if n == 0 || width < 8 {
		return nil
	}
	widths := make([]int, n)
	measure := func(cells []string) {
		for j, c := range cells {
			if w := ansi.StringWidth(c); w > widths[j] {
				widths[j] = w
			}
		}
	}
	measure(tbl.header)
	for _, r := range tbl.rows {
		measure(r)
	}
	// "| a | b |" costs sum(widths) + 3*n + 1 cells (one leading pipe,
	// each column padded by a space on both sides plus its pipe).
	total := func() int {
		s := 1
		for _, w := range widths {
			s += w + 3
		}
		return s
	}
	for total() > width {
		widest, idx := 0, -1
		for j, w := range widths {
			if w > widest {
				widest, idx = w, j
			}
		}
		if widest <= 1 { // every column at its floor and still too wide
			return nil
		}
		widths[idx]--
	}

	row := func(cells []string, head bool) string {
		var b strings.Builder
		b.WriteString("|")
		for j := 0; j < n; j++ {
			cell := fitCell(cells[j], widths[j])
			if head {
				cell = tableHeadStyle.Render(cell)
			}
			b.WriteString(" " + padCell(cell, widths[j], tbl.align[j]) + " |")
		}
		return b.String()
	}

	out := make([]string, 0, len(tbl.rows)+2)
	out = append(out, row(tbl.header, true))
	var rule strings.Builder
	rule.WriteString("|")
	for j := 0; j < n; j++ {
		rule.WriteString(strings.Repeat("-", widths[j]+2) + "|")
	}
	out = append(out, tableRuleStyle.Render(rule.String()))
	for _, r := range tbl.rows {
		out = append(out, row(r, false))
	}
	return out
}

// fitCell truncates s to w display cells with an ellipsis (ANSI-aware
// cut, so a styled cell keeps its reset).
func fitCell(s string, w int) string {
	if ansi.StringWidth(s) <= w {
		return s
	}
	if w <= 1 {
		return "…"
	}
	return ansi.Cut(s, 0, w-1) + "…"
}

// padCell pads s to w display cells per the column alignment. The
// padding lands OUTSIDE any style span, so colors never bleed into the
// gap.
func padCell(s string, w int, a tableAlign) string {
	gap := w - ansi.StringWidth(s)
	if gap <= 0 {
		return s
	}
	switch a {
	case alignRight:
		return strings.Repeat(" ", gap) + s
	case alignCenter:
		l := gap / 2
		return strings.Repeat(" ", l) + s + strings.Repeat(" ", gap-l)
	default:
		return s + strings.Repeat(" ", gap)
	}
}
