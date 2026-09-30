package tui

import (
	"strings"
	"testing"

	"scode/internal/cli"
	"scode/internal/mcp"
)

// runeCol reports the rune column (≈ display column for these
// single-cell glyphs) where sub starts in s, -1 when absent. Byte
// offsets would lie: the box runes are multi-byte.
func runeCol(s, sub string) int {
	i := strings.Index(s, sub)
	if i < 0 {
		return -1
	}
	return len([]rune(s[:i]))
}

// titleCol returns the column where the box title text starts (every
// popup's content must align with it: the rule reads "╭─ title", so the
// title sits three columns in, border included).
func titleCol(lines []string, marker string) int {
	for _, l := range lines {
		if strings.Contains(l, "╭") {
			if c := runeCol(l, marker); c >= 0 {
				return c
			}
		}
	}
	return -1
}

// colOf reports the column where s starts on its row (the row is found
// by content, not position — box chrome shifts with layout).
func colOf(lines []string, s string) int {
	for _, l := range lines {
		if c := runeCol(l, s); c >= 0 {
			return c
		}
	}
	return -1
}

// blankBetween reports whether the rows holding a and b have only
// filler (borders/spaces) between them — the popups' line leading.
func blankBetween(lines []string, a, b string) bool {
	ia, ib := -1, -1
	for i, l := range lines {
		if ia < 0 && strings.Contains(l, a) {
			ia = i
		}
		if strings.Contains(l, b) {
			ib = i
		}
	}
	if ia < 0 || ib <= ia {
		return false
	}
	for _, l := range lines[ia+1 : ib] {
		if strings.Trim(l, "│ ") != "" {
			return false
		}
	}
	return true
}

// Popup content aligns under the box title, and list rows get a blank
// line of leading between them (the terminal has no real line height —
// the gap is the spacing).
func TestPopupAlignsAndSpaces(t *testing.T) {
	m := newTestModel()
	m.width, m.height = 80, 24

	// MCP list: two servers, second highlighted.
	m.mcpOpen = true
	m.mcpServers = []cli.MCPServerInfo{
		{Name: "alpha-srv", Config: mcp.ServerConfig{Transport: "stdio", Command: "x"}},
		{Name: "beta-srv", Config: mcp.ServerConfig{Transport: "http", URL: "http://x"}},
	}
	m.mcpListIdx = 1
	lines := strings.Split(plain(m.mcpView()), "\n")
	tc := titleCol(lines, "MCP 服务器")
	if tc != 3 {
		t.Fatalf("title column = %d, want 3:\n%s", tc, strings.Join(lines, "\n"))
	}
	// Plain row's status dot and the hint text sit ON the title column;
	// the highlighted row's "> " cursor marker rides the gutter to its
	// left, so its content still starts on the title column.
	if c := colOf(lines, "● alpha-srv"); c != tc {
		t.Fatalf("plain row dot at %d, want %d (title column):\n%s", c, tc, strings.Join(lines, "\n"))
	}
	if c := colOf(lines, "● beta-srv"); c != tc {
		t.Fatalf("highlighted row dot at %d, want %d (content must align, marker rides the gutter):\n%s", c, tc, strings.Join(lines, "\n"))
	}
	if c := colOf(lines, "> ● beta-srv"); c != 1 {
		t.Fatalf("highlight marker at %d, want the gutter at 1:\n%s", c, strings.Join(lines, "\n"))
	}
	if c := colOf(lines, "↑/↓ 选择"); c != tc {
		t.Fatalf("hint at %d, want %d:\n%s", c, tc, strings.Join(lines, "\n"))
	}
	if !blankBetween(lines, "alpha-srv", "beta-srv") {
		t.Fatalf("no leading between list rows:\n%s", strings.Join(lines, "\n"))
	}
	// The frame pads one blank line under the title rule.
	if strings.Trim(lines[1], "│ ") != "" {
		t.Fatalf("no padding row under the title rule:\n%s", strings.Join(lines, "\n"))
	}

	// MCP edit form: focused/unfocused field labels share the column.
	m.mcpNew()
	lines = strings.Split(plain(m.mcpView()), "\n")
	tc = titleCol(lines, "MCP 新建")
	if tc < 0 {
		t.Fatalf("edit view missing title:\n%s", strings.Join(lines, "\n"))
	}
	if c := colOf(lines, "> 名称"); c != 1 {
		t.Fatalf("focused field marker at %d, want the gutter at 1:\n%s", c, strings.Join(lines, "\n"))
	}
	if c := colOf(lines, "名称"); c != tc {
		t.Fatalf("focused field label at %d, want %d:\n%s", c, tc, strings.Join(lines, "\n"))
	}
	if c := colOf(lines, "传输"); c != tc { // unfocused: the 2-space indent IS the title column
		t.Fatalf("unfocused field at %d, want %d:\n%s", c, tc, strings.Join(lines, "\n"))
	}

	// Tool approval: body text and buttons align with the title too.
	m2 := newTestModel()
	m2.width, m2.height = 80, 24
	m2.pending = &approvalRequest{kind: "tool", title: "approval required", body: "bash command: rm -rf /tmp/x",
		hint: "[y] [n] [a] [p]", answer: make(chan string, 1)}
	lines = strings.Split(plain(m2.approvalView()), "\n")
	tc = titleCol(lines, "approval required")
	if c := colOf(lines, "bash command"); c != tc {
		t.Fatalf("approval body at %d, want %d:\n%s", c, tc, strings.Join(lines, "\n"))
	}
	for _, b := range approvalButtons("tool") {
		if c := colOf(lines, b.Label); c < tc {
			t.Fatalf("button %q before the title column (%d < %d):\n%s", b.Label, c, tc, strings.Join(lines, "\n"))
		}
	}
}
