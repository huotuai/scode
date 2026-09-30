package tui

import (
	"fmt"
	"strings"

	"scode/internal/plantrack"
	"scode/internal/session"
)

// planBlock renders the live plan checklist — the TUI's Claude-Code
// style progress display. The header carries the done-count and the
// current step (进行到哪一步 at a glance); completed steps check
// through dim and struck, the in-progress step leads in the accent
// color, pending steps dim only their marker.
func planBlock(plan session.PlanEntry, width int) string {
	stepCap := max(10, width-gutterText-2) // gutter + 2-cell marker
	done, cur := 0, -1
	rows := make([]string, 0, len(plan.Items)+1)
	for i, it := range plan.Items {
		switch it.Status {
		case plantrack.Completed:
			done++
			rows = append(rows, gutterPad+dimStyle.Render("✔ ")+dimStyle.Strikethrough(true).Render(truncate(it.Step, stepCap)))
		case plantrack.InProgress:
			cur = i
			rows = append(rows, gutterPad+userStyle.Render("◐ ")+userStyle.Render(truncate(it.Step, stepCap)))
		default:
			rows = append(rows, gutterPad+dimStyle.Render("○ ")+truncate(it.Step, stepCap))
		}
	}
	head := fmt.Sprintf("计划 %d/%d", done, len(plan.Items))
	if cur >= 0 {
		head += " · " + plan.Items[cur].Step
	}
	var b strings.Builder
	b.WriteString(gutterView(statusPlanStyle, truncate(head, max(10, width-gutterText))))
	if plan.Explanation != "" {
		b.WriteString("\n" + gutterPad + dimStyle.Render(truncate(plan.Explanation, max(10, width-gutterText))))
	}
	b.WriteString("\n" + strings.Join(rows, "\n"))
	return b.String()
}
