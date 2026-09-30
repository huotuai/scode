package tui

import (
	"net/http"
	"strings"
	"testing"

	"scode/internal/agent"
	"scode/internal/llm"
)

// A failed tool's message renders as a hanging-indented red excerpt:
// every visual row — wrapped continuations included — sits on the
// transcript's text column. Windows shell errors arrive multi-line
// with blank-line padding; the old single-line rendering dropped their
// continuations onto column 0.
func TestErrorExcerptAlignment(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	m := newModel(app, make(chan any, 4))
	m.width, m.height = 40, 24 // narrow: forces CJK wrapping

	call := llm.Block{Kind: llm.BlockToolCall, ID: "t1", Name: "bash", Arguments: []byte(`{"command":"head x"}`)}
	m.renderEvent(agent.Event{Type: agent.EvToolStart, Call: &call})
	failed := agent.ErrorResult("文件名、目录名或卷标语法不正确。\n---\n" +
		"'head' 不是内部或外部命令，也不是可运行的程序\n或批处理文件。\n\n" +
		"Command exited with code 1")
	m.renderEvent(agent.Event{Type: agent.EvToolEnd, Call: &call, Result: &failed})

	// The excerpt is the block after the tool row.
	excerpt := m.blocks[len(m.blocks)-1]
	view := plain(strings.Join(excerpt.lines, "\n"))
	lines := strings.Split(view, "\n")
	if len(lines) < 5 {
		t.Fatalf("excerpt too short (multi-line message collapsed):\n%s", view)
	}
	if !strings.HasPrefix(lines[0], gutterPad+"✗") {
		t.Fatalf("first line missing the ✗ gutter prefix:\n%q", lines[0])
	}
	for i, l := range lines {
		if !strings.HasPrefix(l, gutterPad) {
			t.Fatalf("line %d fell out of the gutter (column %d):\n%q\n--- full:\n%s", i, indentOf(l), l, view)
		}
		if cells := lipglossWidth(l); cells > m.width {
			t.Fatalf("line %d exceeds the width (%d > %d):\n%q", i, cells, m.width, l)
		}
	}
	// Blank-line padding is collapsed; the wrapped CJK continuation
	// stays inside the excerpt ("或批处理文件。" is part of it).
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			t.Fatalf("padding line survived:\n%s", view)
		}
	}
	if !strings.Contains(view, "或批处理文件") {
		t.Fatalf("wrapped continuation lost:\n%s", view)
	}
}

func indentOf(l string) int {
	n := 0
	for _, r := range l {
		if r == ' ' {
			n++
		} else {
			break
		}
	}
	return n
}

func lipglossWidth(s string) int {
	return len([]rune(stripANSI(s)))
}
