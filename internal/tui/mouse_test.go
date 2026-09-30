package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// firstVisible mirrors view()'s clamp; lineAt spans committed lines and
// the live tail.
func TestScrollerFirstVisibleAndLineAt(t *testing.T) {
	s := scroller{height: 2, pinned: true}
	s.appendLines("a", "b", "c") // total 3, maxUp 1
	if got := s.firstVisible(); got != 1 {
		t.Fatalf("pinned firstVisible = %d, want maxUp 1", got)
	}
	s.scrollUp(1) // unpins, yOffset = maxUp-1 = 0
	if got := s.firstVisible(); got != 0 {
		t.Fatalf("scrolled firstVisible = %d, want 0", got)
	}
	s.yOffset = 5 // stale beyond maxUp (content shrank)
	if got := s.firstVisible(); got != 1 {
		t.Fatalf("over-scrolled firstVisible = %d, want clamped 1", got)
	}

	s.setLive([]string{"L"})
	if ln, ok := s.lineAt(2); !ok || ln != "c" {
		t.Fatalf("lineAt(2) = %q,%v want committed c", ln, ok)
	}
	if ln, ok := s.lineAt(3); !ok || ln != "L" {
		t.Fatalf("lineAt(3) = %q,%v want live L", ln, ok)
	}
	if _, ok := s.lineAt(4); ok {
		t.Fatal("lineAt(4) should be out of range")
	}
	if _, ok := s.lineAt(-1); ok {
		t.Fatal("lineAt(-1) should be out of range")
	}
}

// The full selection flow: left-drag highlights the span (ANSI-aware
// splice, absolute line anchors), release keeps it, right click copies
// the plain text and clears.
func TestMouseSelectionAndCopy(t *testing.T) {
	m := newTestModel()
	for _, s := range []string{"alpha line", "beta line", "gamma line"} {
		m.appendBlock(s)
		m.ensureSpacer() // first item never leads with a gap: alpha is row 0
	}
	// Blocks: [alpha, spacer, beta, spacer, gamma]; height 20 swallows
	// everything, pinned, firstVisible 0. Row 0 = alpha, row 2 = beta.
	if strings.Contains(m.renderViewport(), "\x1b[7m") {
		t.Fatal("highlight before any selection")
	}

	press := tea.MouseClickMsg{X: 2, Y: 0, Button: tea.MouseLeft}
	tm, _ := m.Update(press)
	m = tm.(model)
	if !m.sel.active {
		t.Fatal("press did not start a selection")
	}
	tm, _ = m.Update(tea.MouseMotionMsg{X: 6, Y: 2, Button: tea.MouseLeft})
	m = tm.(model)
	if v := m.renderViewport(); !strings.Contains(v, "\x1b[7m") {
		t.Fatalf("drag produced no highlight:\n%s", v)
	}

	// Release keeps a non-zero-width selection highlighted.
	tm, _ = m.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft})
	m = tm.(model)
	if !m.sel.active || m.sel.zeroWidth() {
		t.Fatal("release dropped a dragged selection")
	}

	// Right click copies: endpoint column cuts, ANSI stripped, one line
	// per wrapped row.
	orig := writeClipboardTextFn
	t.Cleanup(func() { writeClipboardTextFn = orig })
	var got string
	writeClipboardTextFn = func(s string) bool { got = s; return true }
	tm, _ = m.Update(tea.MouseClickMsg{X: 1, Y: 1, Button: tea.MouseRight})
	m = tm.(model)
	if want := "pha line\n\nbeta l"; got != want { // cut [0,6) of "beta line" is 6 cells
		t.Fatalf("copied = %q, want %q", got, want)
	}
	if m.sel.active {
		t.Fatal("copy left the selection active")
	}
	if v := m.renderViewport(); strings.Contains(v, "\x1b[7m") {
		t.Fatal("highlight survived the copy")
	}
	// The outcome is a transient top-right toast, not a transcript note.
	if m.toast == nil || !strings.Contains(m.toast.text, "已复制") {
		t.Fatalf("copy toast missing: %+v", m.toast)
	}
	composited := m.compositeToast("first line\nsecond line")
	if top := strings.Split(plainText(composited), "\n")[0]; !strings.Contains(top, "已复制") {
		t.Fatalf("toast not composited: %q", top)
	}
	// A stale expiry tick (older toast) clears nothing; the toast's own
	// tick clears it.
	id := m.toast.id
	tm, _ = m.Update(toastExpireMsg{id: id + 100})
	m = tm.(model)
	if m.toast == nil {
		t.Fatal("stale expiry tick cleared the current toast")
	}
	tm, _ = m.Update(toastExpireMsg{id: id})
	m = tm.(model)
	if m.toast != nil {
		t.Fatal("expiry did not clear the toast")
	}
}

// A plain click (press + release, no drag) selects nothing.
func TestMousePlainClickClears(t *testing.T) {
	m := newTestModel()
	m.ensureSpacer()
	m.appendBlock("alpha line")
	tm, _ := m.Update(tea.MouseClickMsg{X: 2, Y: 1, Button: tea.MouseLeft})
	m = tm.(model)
	tm, _ = m.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft})
	m = tm.(model)
	if m.sel.active {
		t.Fatal("zero-width selection survived release")
	}
	if strings.Contains(m.renderViewport(), "\x1b[7m") {
		t.Fatal("zero-width selection highlighted")
	}
}

// Selection anchors are absolute line numbers: scrolling mid-drag
// keeps the span on the same text.
func TestMouseSelectionFollowsScroll(t *testing.T) {
	m := newTestModel()
	for i := range 40 {
		m.appendBlock(fmt.Sprintf("row-%02d", i))
		m.ensureSpacer() // 40 rows + 40 gaps = 80 lines
	}
	// Height 20, pinned: firstVisible = maxUp = 60. Press on the top
	// visible row, wheel up 3, extend two rows in — the anchor stays put
	// while the head lands one line above it.
	tm, _ := m.Update(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	m = tm.(model)
	if m.sel.anchorLine != 60 {
		t.Fatalf("anchor = %d, want 60 (maxUp)", m.sel.anchorLine)
	}
	tm, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	m = tm.(model)
	tm, _ = m.Update(tea.MouseMotionMsg{X: 2, Y: 2, Button: tea.MouseLeft})
	m = tm.(model)
	l0, _, l1, _ := m.sel.normalized()
	if l0 != 59 || l1 != 60 {
		t.Fatalf("span = %d..%d, want 59..60", l0, l1)
	}
}

// Clicks below the viewport (input box, status bar, overlays) never
// start a selection.
func TestMouseClickBelowViewportIgnored(t *testing.T) {
	m := newTestModel()
	m.ensureSpacer()
	m.appendBlock("alpha line")
	tm, _ := m.Update(tea.MouseClickMsg{X: 2, Y: m.vp.Height(), Button: tea.MouseLeft})
	m = tm.(model)
	if m.sel.active {
		t.Fatal("click below the viewport started a selection")
	}
}

// The toast composites at the top-right corner: padded when the first
// line leaves room, covering the tail of a full-width line otherwise —
// and never wider than the screen.
func TestToastComposite(t *testing.T) {
	m := newTestModel()    // width 80
	m.showToast("已复制 3 行") // cmd discarded: expiry is asserted separately

	short := m.compositeToast("hello\nworld")
	top := strings.Split(plainText(short), "\n")[0]
	if !strings.HasPrefix(top, "hello") || !strings.HasSuffix(strings.TrimRight(top, " "), "已复制 3 行") {
		t.Fatalf("short-line toast = %q", top)
	}
	if w := ansi.StringWidth(top); w > 80 {
		t.Fatalf("short-line toast width %d > 80", w)
	}

	full := m.compositeToast(strings.Repeat("x", 80) + "\nworld")
	top = strings.Split(plainText(full), "\n")[0]
	if !strings.HasPrefix(top, "x") || !strings.HasSuffix(strings.TrimRight(top, " "), "已复制 3 行") {
		t.Fatalf("full-line toast = %q", top)
	}
	if w := ansi.StringWidth(top); w > 80 {
		t.Fatalf("full-line toast width %d > 80", w)
	}
}
