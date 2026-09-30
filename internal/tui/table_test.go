package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"scode/internal/agent"
	"scode/internal/llm"
)

// stripped joins rendered lines as plain text (styles off) for
// content assertions.
func stripped(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = ansi.Strip(l)
	}
	return out
}

func TestRenderTableBasic(t *testing.T) {
	text := "before\n| Name | Age |\n| ---- | --- |\n| Alice | 30 |\n| 李雷 | 7 |\nafter"
	got := stripped(renderTablesInText(text, 40))
	want := []string{
		"before",
		"| Name  | Age |",
		"|-------|-----|",
		"| Alice | 30  |",
		"| 李雷  | 7   |",
		"after",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// CJK cells pad by display width: every row the same cell count.
	for i, l := range got[1:5] {
		if w := ansi.StringWidth(l); w != 15 {
			t.Fatalf("row %d width = %d, want 15: %q", i, w, l)
		}
	}
}

func TestRenderTableAlignment(t *testing.T) {
	text := "| L | R | C |\n| :--- | ---: | :-: |\n| a | 1 | x |\n| bbb | 22 | yy |"
	got := stripped(renderTablesInText(text, 60))
	want := []string{
		"| L   |  R | C  |",
		"|-----|----|----|",
		"| a   |  1 | x  |",
		"| bbb | 22 | yy |",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestRenderTableEscapedPipe(t *testing.T) {
	text := `| Expr | Note |
| --- | --- |
| a \| b | pipe |`
	got := stripped(renderTablesInText(text, 40))
	if len(got) != 3 || !strings.Contains(got[2], "a | b") {
		t.Fatalf("escaped pipe must stay inside one cell:\n%s", strings.Join(got, "\n"))
	}
}

func TestRenderTableRaggedRows(t *testing.T) {
	// Short and long rows normalize to the widest column count.
	text := "| a | b |\n| --- | --- |\n| 1 |\n| 1 | 2 | 3 |"
	got := stripped(renderTablesInText(text, 40))
	want := []string{
		"| a | b |   |",
		"|---|---|---|",
		"| 1 |   |   |",
		"| 1 | 2 | 3 |",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestRenderTableShrinkToWidth(t *testing.T) {
	text := "| Package | Description |\n| --- | --- |\n| internal/agent | the agent loop, streaming turns and tool execution |"
	got := stripped(renderTablesInText(text, 40))
	for _, l := range got {
		if w := ansi.StringWidth(l); w > 40 {
			t.Fatalf("row over width: %d %q", w, l)
		}
	}
	if !strings.Contains(got[2], "…") {
		t.Fatalf("long cell should truncate with an ellipsis:\n%s", strings.Join(got, "\n"))
	}
}

func TestRenderTableFallbackWhenTooNarrow(t *testing.T) {
	text := "| aa | bb |\n| --- | --- |\n| 1 | 2 |"
	got := renderTablesInText(text, 7) // below the minimal table layout
	if strings.Join(got, "\n") != text {
		t.Fatalf("unrenderable table must fall back to the raw lines:\n%s", strings.Join(got, "\n"))
	}
}

func TestNonTableLinesPassThrough(t *testing.T) {
	// No delimiter row, a bare horizontal rule, a pipe-free paragraph.
	text := "a | b\n\n---\n\nplain text"
	got := renderTablesInText(text, 40)
	if strings.Join(got, "\n") != text {
		t.Fatalf("non-table text changed:\n%s", strings.Join(got, "\n"))
	}
}

func TestTableInsideCodeFenceStaysLiteral(t *testing.T) {
	text := "```\n| a | b |\n| --- | --- |\n| 1 | 2 |\n```"
	got := renderTablesInText(text, 40)
	if strings.Join(got, "\n") != text {
		t.Fatalf("fenced code must stay literal:\n%s", strings.Join(got, "\n"))
	}
}

// A text block carrying a table flushes into an aligned table under the
// content dot, and keeps its raw text for re-rendering.
func TestFlushLiveRendersTable(t *testing.T) {
	m := newTestModel()
	md := "结果:\n| 名称 | 数量 |\n| --- | ---: |\n| 苹果 | 3 |\n| 香蕉 | 12 |"
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextDelta, Delta: md}})
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextEnd}})

	if len(m.blocks) != 1 || !m.blocks[0].isText {
		t.Fatalf("blocks = %#v", m.blocks)
	}
	b := m.blocks[0]
	if b.text != md {
		t.Fatalf("raw text not kept: %q", b.text)
	}
	plain := plain(b.rendered)
	if !strings.Contains(plain, "| 名称 | 数量 |") || !strings.Contains(plain, "| 香蕉 |   12 |") {
		t.Fatalf("table not aligned:\n%s", plain)
	}
	// The header row is bold, the delimiter row dimmed.
	if !strings.Contains(b.rendered, "\x1b[1m") {
		t.Fatalf("header should be bold:\n%q", b.rendered)
	}
}

// A width change re-renders committed text blocks from their raw text:
// the table re-aligns to the narrower width instead of hard-cutting.
func TestTableRealignsOnResize(t *testing.T) {
	m := newTestModel()
	md := "| Package | Description |\n| --- | --- |\n| internal/agent | the agent loop and tool execution |"
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextDelta, Delta: md}})
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextEnd}})

	m.width = 50
	m.rebuildRendered()
	for _, l := range m.blocks[0].lines {
		if w := ansi.StringWidth(l); w > 50 {
			t.Fatalf("rebuilt line over width: %d %q", w, l)
		}
	}
	plain := plain(m.blocks[0].rendered)
	if !strings.Contains(plain, "…") {
		t.Fatalf("narrowed table should truncate cells:\n%s", plain)
	}
}

// Non-table text flushes EXACTLY as before the table support: same
// wrap, same gutter — the upgrade only fires on pipe tables.
func TestFlushLivePlainTextUnchanged(t *testing.T) {
	m := newTestModel()
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextDelta, Delta: "hello world"}})
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextEnd}})
	if got := plain(m.blocks[0].rendered); got != " ● hello world" {
		t.Fatalf("rendered = %q", got)
	}
}
