package tui

// Tool-call presentation: the one-line row summary (what the call IS)
// plus the post-completion detail preview rendered under the row (what
// it DID): bash shows its command and an output preview, write the
// written lines, edit a colorized unified diff (the edit tool's result
// already embeds one), read/ls/grep/find a result preview. Everything
// is capped with an overflow counter so a chatty tool never floods the
// transcript; failures keep the single ✗ excerpt (no preview).

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"scode/internal/agent"
	"scode/internal/i18n"
	"scode/internal/llm"
	"scode/internal/subagent"
)

// taskReportPreview renders the first substantive line of a delegate's
// report under its row (the transcript hint; the model gets the whole
// thing as the tool result).
func taskReportPreview(text string, w int) []string {
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, subagent.ReportFooterPrefix) {
			return []string{dimStyle.Render(ansi.Truncate(l, w, "…"))}
		}
	}
	return nil
}

// toolRowName is the row's display name: the delegation tool names its
// delegate (Claude Code's Task(agent) shape), everything else renders
// the capitalized tool name.
func toolRowName(call *llm.Block) string {
	if call.Name != subagent.ToolName {
		return capitalize(call.Name)
	}
	var a struct {
		Agent string `json:"agent"`
	}
	_ = json.Unmarshal(call.Arguments, &a)
	if a.Agent == "" {
		return i18n.T("tui.tool.subagent")
	}
	return i18n.Tf("tui.tool.subagentNamed", a.Agent)
}

// toolArgs is the best-effort decode of a call's arguments — every
// field optional; unknown tools fall back to the raw-args summary.
type toolArgs struct {
	Command string `json:"command"`
	Path    string `json:"path"`
	Pattern string `json:"pattern"`
	URL     string `json:"url"`
	Query   string `json:"query"`
	Offset  int    `json:"offset"`
	Limit   int    `json:"limit"`
	Content string `json:"content"`
	Agent   string `json:"agent"`
	Prompt  string `json:"prompt"`
	Edits   []struct {
		OldText string `json:"oldText"`
		NewText string `json:"newText"`
	} `json:"edits"`
}

// Diff/preview styles: the edit preview's red/green text are the
// reference design's left-bar colors (#E03131 deletions, #1E8A3E
// additions); hunk headers cyan, context and ordinary previews dim
// (the transcript's quiet voice).
var (
	diffAddStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#1E8A3E"))
	diffDelStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#E03131"))
	diffHunkStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
)

// toolPreviewMax caps the generic output preview; diffs get more room
// (toolDiffMax) because the change IS the information.
const (
	toolPreviewMax = 10
	toolDiffMax    = 24
)

// toolSummary renders the human one-liner for the tool row (what the
// call does), replacing the raw-JSON args blob.
func toolSummary(name string, raw []byte) string {
	var a toolArgs
	_ = json.Unmarshal(raw, &a) // best effort; malformed args fall through
	switch name {
	case "bash":
		return truncateRunes(oneLine(a.Command), 96)
	case "read":
		s := shortPath(a.Path)
		if a.Offset > 1 {
			s += fmt.Sprintf(" :%d", a.Offset)
		}
		return s
	case "write":
		return i18n.Tf("tui.tool.writeSummary", shortPath(a.Path), lineCount(a.Content))
	case "edit":
		n := len(a.Edits)
		if n == 0 {
			return shortPath(a.Path)
		}
		return i18n.Tf("tui.tool.editSummary", shortPath(a.Path), n)
	case "ls":
		return shortPath(orDot(a.Path))
	case "grep":
		s := truncateRunes(a.Pattern, 48)
		if a.Path != "" {
			s += " · " + shortPath(a.Path)
		}
		return s
	case "find":
		s := truncateRunes(a.Pattern, 48)
		if a.Path != "" {
			s += " · " + shortPath(a.Path)
		}
		return s
	case subagent.ToolName:
		return "「" + truncateRunes(oneLine(a.Prompt), 56) + "」"
	case "web_fetch":
		return truncateRunes(oneLine(a.URL), 96)
	case "web_search":
		return "🔎 " + truncateRunes(oneLine(a.Query), 92)
	}
	return briefArgs(raw)
}

// toolDetail renders the post-completion preview under the row: styled
// lines, each already indented to the transcript's text column. Nil
// for failures (the ✗ excerpt carries those) and for tools with
// nothing worth showing. width is the transcript width: lines pre-wrap
// with a hanging indent so wrapped continuations stay on the text
// column instead of falling back to the left edge.
func toolDetail(name string, raw []byte, res *agent.ToolResult, width int) []string {
	if res == nil || res.IsError {
		return nil
	}
	text := resultText(res)
	var a toolArgs
	_ = json.Unmarshal(raw, &a)
	w := max(10, width-gutterText)
	switch name {
	case "edit":
		return diffPreview(editDiff(text), w)
	case "write":
		return contentPreview(a.Content, w)
	case "bash", "read", "ls", "grep", "find", "web_fetch", "web_search":
		return outputPreview(text, w)
	case subagent.ToolName:
		// The delegate's report preview: the first finding line (the
		// full report rides back to the model as the tool result).
		return taskReportPreview(text, w)
	}
	return nil
}

// toolRowView renders the status row with a hanging indent: the
// status dot and tool name lead, an over-long summary wraps onto the
// text column instead of the left edge. The first segment's budget
// also pays for the dot column and the tool name, so no visual row
// exceeds the transcript width.
func toolRowView(dot lipgloss.Style, name, summary string, width int) string {
	lead := lipgloss.Width(capitalize(name)) + 2 // "Name  " after the dot column
	first := max(10, width-gutterText-lead)
	rest := max(10, width-gutterText)
	var segs []string
	total := ansi.StringWidth(summary)
	for idx, step := 0, first; idx < total; idx, step = idx+step, rest {
		segs = append(segs, ansi.Cut(summary, idx, idx+step))
	}
	if len(segs) == 0 {
		segs = []string{""}
	}
	rows := make([]string, 0, len(segs))
	rows = append(rows, toolLine(dot, name, segs[0]))
	for _, s := range segs[1:] {
		rows = append(rows, gutterPad+dimStyle.Render(s))
	}
	return strings.Join(rows, "\n")
}

// editDiff extracts the unified diff from the edit tool's result text
// (the "Successfully replaced …" header line is dropped — the row
// summary already says it).
func editDiff(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "--- ") {
			return strings.Join(lines[i:], "\n")
		}
	}
	// No diff body (e.g. the "diff omitted" summary): nothing to show
	// beyond the summary line.
	return ""
}

// diffPreview colorizes a unified diff: + green, - red, @@ cyan,
// file headers and context dim, capped with an overflow counter.
func diffPreview(diff string, w int) []string {
	lines := strings.Split(strings.TrimRight(diff, "\n"), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return nil
	}
	return hang(lines, diffLineStyle, w, toolDiffMax)
}

// diffLineStyle picks a diff line's color from its leading marker.
func diffLineStyle(l string) lipgloss.Style {
	switch {
	case strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---"):
		return dimStyle
	case strings.HasPrefix(l, "@@"):
		return diffHunkStyle
	case strings.HasPrefix(l, "+"):
		return diffAddStyle
	case strings.HasPrefix(l, "-"):
		return diffDelStyle
	}
	return dimStyle
}

// contentPreview shows what a write put into the file: the first lines
// dim, overflow counted against the full content.
func contentPreview(content string, w int) []string {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return nil
	}
	return hang(lines, dimLineStyle, w, 8)
}

// outputPreview shows the head of a tool's text output (bash stdout,
// read windows, listings, grep hits), capped with an overflow counter.
func outputPreview(text string, w int) []string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	// Leading blank lines carry no information (bash output often
	// starts with one).
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	if len(lines) == 0 {
		return nil
	}
	return hang(lines, dimLineStyle, w, toolPreviewMax)
}

// dimLineStyle renders every preview line in the transcript's quiet
// voice (constant style, the hang signature's shape).
func dimLineStyle(string) lipgloss.Style { return dimStyle }

// hang renders raw lines as hanging-indent preview rows: every line
// wraps at w cells and EVERY visual row (continuations included) sits
// on the transcript's text column, styled per source line. The cap
// counts visual rows; unrendered source lines collapse into an
// overflow counter.
func hang(lines []string, style func(string) lipgloss.Style, w, maxVis int) []string {
	var out []string
	hidden := 0
	for i, l := range lines {
		segs := wrapLine(l, w)
		if len(out)+len(segs) > maxVis {
			hidden = len(lines) - i
			break
		}
		st := style(l)
		for _, s := range segs {
			out = append(out, gutterPad+st.Render(s))
		}
	}
	if hidden > 0 {
		out = append(out, gutterPad+dimStyle.Render(i18n.Tf("tui.tool.moreLines", hidden)))
	}
	return out
}

// errExcerptMax caps the failed-result excerpt's source lines.
const errExcerptMax = 6

// errExcerpt renders a failed tool's message as a red hanging-indent
// excerpt: leading ✗ on the first line, EVERY visual row (wrapped
// continuations included) on the transcript's text column. Shell
// errors arrive multi-line with blank-line padding — collapsed here so
// the excerpt reads as one tight block.
func errExcerpt(msg string, w int) []string {
	msg = strings.ReplaceAll(strings.TrimRight(msg, "\n"), "\r\n", "\n")
	var tight []string
	for _, l := range strings.Split(msg, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue // shell-output padding carries no information
		}
		tight = append(tight, l)
	}
	if len(tight) == 0 {
		tight = []string{i18n.T("tui.tool.noErrOutput")}
	}
	if len(tight) > errExcerptMax {
		tight = append(tight[:errExcerptMax:errExcerptMax],
			i18n.Tf("tui.tool.moreLines", len(tight)-errExcerptMax))
	}
	var out []string
	for i, l := range tight {
		if i == 0 {
			l = "✗ " + l
		}
		for _, s := range wrapLine(l, max(20, w-gutterText)) {
			out = append(out, gutterPad+errStyle.Render(s))
		}
	}
	return out
}

// oneLine folds a multi-line command onto the summary row.
func oneLine(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", " ⏎ ")
}

// lineCount counts the content's lines (write summary).
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(s, "\n"), "\n") + 1
}

// orDot defaults an empty path to the working directory marker.
func orDot(p string) string {
	if p == "" {
		return "."
	}
	return p
}

// shortPath keeps a path readable on the row: home collapsed, long
// paths tail-truncated.
func shortPath(p string) string {
	return truncateLeft(collapseHome(p), 64)
}
