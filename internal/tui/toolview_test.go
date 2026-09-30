package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"scode/internal/agent"
	"scode/internal/llm"
)

// The tool row tells what the call IS: the bash command, the file a
// write/edit touches (with the change size), the grep pattern — never
// the raw JSON blob.
func TestToolSummary(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"bash", `{"command":"git status --short"}`, "git status --short"},
		{"bash", "{\"command\":\"ls\\nps\"}", "ls ⏎ ps"},
		{"read", `{"path":"a/b.go"}`, "a/b.go"},
		{"read", `{"path":"a/b.go","offset":40}`, "a/b.go :40"},
		{"write", `{"path":"x.txt","content":"l1\nl2\nl3"}`, "x.txt (写入 3 行)"},
		{"edit", `{"path":"x.go","edits":[{"oldText":"a","newText":"b"},{"oldText":"c","newText":"d"}]}`, "x.go (2 处修改)"},
		{"ls", `{"path":"internal"}`, "internal"},
		{"ls", `{}`, "."},
		{"grep", `{"pattern":"func main","path":"cmd"}`, "func main · cmd"},
		{"find", `{"pattern":"**/*.go"}`, "**/*.go"},
	}
	for _, c := range cases {
		if got := toolSummary(c.name, []byte(c.args)); got != c.want {
			t.Errorf("toolSummary(%s) = %q, want %q", c.name, got, c.want)
		}
	}
	// Unknown tools keep the raw-args fallback.
	if got := toolSummary("mcp_x", []byte(`{"foo":1}`)); !strings.Contains(got, "foo") {
		t.Errorf("unknown tool fallback = %q", got)
	}
}

// settle runs a tool start+end pair through renderEvent and returns the
// settled block's rendered text (ANSI kept).
func settle(m model, name, args string, res agent.ToolResult) string {
	call := llm.Block{Kind: llm.BlockToolCall, ID: "c1", Name: name, Arguments: []byte(args)}
	m.renderEvent(agent.Event{Type: agent.EvToolStart, Call: &call})
	m.renderEvent(agent.Event{Type: agent.EvToolEnd, Call: &call, Result: &res})
	return m.blocks[0].rendered
}

// edit renders a colorized unified diff under the row: deletions and
// additions render as text in the soft pastel bar colors, hunk
// headers cyan.
func TestToolDetailEditDiff(t *testing.T) {
	res := agent.TextResult("Successfully replaced 1 block(s) in x.go\n\n" +
		"--- x.go\n+++ x.go\n@@ -1,2 +1,2 @@\n ctx\n-old line\n+new line\n")
	got := settle(newTestModel(), "edit", `{"path":"x.go","edits":[{"oldText":"old line","newText":"new line"}]}`, res)

	// #E03131 / #1E8A3E text colors.
	if !strings.Contains(got, "\x1b[38;2;224;49;49m-old line") {
		t.Fatalf("deletion not red: %q", got)
	}
	if !strings.Contains(got, "\x1b[38;2;30;138;62m+new line") {
		t.Fatalf("addition not green: %q", got)
	}
	if !strings.Contains(got, "\x1b[36m@@ -1,2 +1,2 @@") {
		t.Fatalf("hunk header not cyan: %q", got)
	}
	text := plainText(got)
	if !strings.Contains(text, "Edit") || !strings.Contains(text, "x.go (1 处修改)") {
		t.Fatalf("row summary missing: %q", text)
	}
	if strings.Contains(text, "Successfully replaced") {
		t.Fatalf("result header leaked into the view: %q", text)
	}
}

// write previews the written content under the row, capped with the
// full line count.
func TestToolDetailWritePreview(t *testing.T) {
	content := "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10\nl11\nl12"
	res := agent.TextResult("Wrote 24 bytes to x.txt")
	got := settle(newTestModel(), "write", `{"path":"x.txt","content":"`+strings.ReplaceAll(content, "\n", `\n`)+`"}`, res)
	text := plainText(got)
	if !strings.Contains(text, "l1") || !strings.Contains(text, "l8") {
		t.Fatalf("content head missing: %q", text)
	}
	if strings.Contains(text, "l9") {
		t.Fatalf("preview not capped: %q", text)
	}
	if !strings.Contains(text, "还有 4 行") {
		t.Fatalf("overflow counter missing: %q", text)
	}
}

// bash previews the output head under the row; a chatty command is
// capped with an overflow counter.
func TestToolDetailBashOutput(t *testing.T) {
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = "out line"
	}
	res := agent.TextResult(strings.Join(lines, "\n"))
	got := settle(newTestModel(), "bash", `{"command":"seq 30"}`, res)
	text := plainText(got)
	if !strings.Contains(text, "Bash") || !strings.Contains(text, "seq 30") {
		t.Fatalf("row summary missing: %q", text)
	}
	if strings.Count(text, "out line") != 10 {
		t.Fatalf("preview not capped at 10: %q", text)
	}
	if !strings.Contains(text, "还有 20 行") {
		t.Fatalf("overflow counter missing: %q", text)
	}
}

// Over-long content wraps with a hanging indent: every visual row —
// the status row's continuation, diff lines, output lines — stays on
// the text column and within the transcript width.
func TestToolViewHangingIndent(t *testing.T) {
	m := newTestModel()
	m.width = 40 // narrow: everything wraps
	longCmd := "echo " + strings.Repeat("x", 120)
	longOut := strings.Repeat("y", 200)
	call := llm.Block{Kind: llm.BlockToolCall, ID: "c1", Name: "bash", Arguments: []byte(`{"command":"` + longCmd + `"}`)}
	res := agent.TextResult(longOut)
	m.renderEvent(agent.Event{Type: agent.EvToolStart, Call: &call})
	m.renderEvent(agent.Event{Type: agent.EvToolEnd, Call: &call, Result: &res})

	lines := m.blocks[0].lines // wrapped at commit time
	if len(lines) < 4 {
		t.Fatalf("expected wrapped rows, got %v", lines)
	}
	for i, l := range lines {
		p := plainText(l)
		if w := lipgloss.Width(p); w > 40 {
			t.Fatalf("row %d exceeds the width (%d > 40): %q", i, w, p)
		}
		if !strings.HasPrefix(p, " ●") && !strings.HasPrefix(p, "   ") {
			t.Fatalf("row %d not on the text column: %q", i, p)
		}
	}
	// The output line's continuation must not fall back to column 0,
	// and no content is lost across the wrapped rows.
	text := ""
	for _, l := range lines {
		text += plainText(l)
	}
	if strings.Count(text, "y") != 200 {
		t.Fatalf("output content lost in wrapping (%d y's): %v", strings.Count(text, "y"), lines)
	}
}

// No output → no preview; failures keep the ✗ excerpt path and never
// render a preview.
func TestToolDetailSkips(t *testing.T) {
	m := newTestModel()
	got := settle(m, "bash", `{"command":"true"}`, agent.TextResult(""))
	if strings.Contains(plainText(got), "还有") {
		t.Fatalf("empty output rendered a preview: %q", got)
	}

	m = newTestModel()
	call := llm.Block{Kind: llm.BlockToolCall, ID: "c2", Name: "edit", Arguments: []byte(`{"path":"x.go","edits":[{"oldText":"a","newText":"b"}]}`)}
	res := agent.ErrorResult("oldText not found in x.go")
	m.renderEvent(agent.Event{Type: agent.EvToolStart, Call: &call})
	m.renderEvent(agent.Event{Type: agent.EvToolEnd, Call: &call, Result: &res})
	if strings.Contains(plainText(m.blocks[0].rendered), "@@") {
		t.Fatalf("failure rendered a diff: %q", m.blocks[0].rendered)
	}
	if got := plainText(m.blocks[1].rendered); !strings.Contains(got, "oldText not found") {
		t.Fatalf("error excerpt missing: %q", got)
	}
}
