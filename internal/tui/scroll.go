package tui

import (
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// scroller is a minimal append-only transcript viewport. The generic
// bubbles viewport re-splits its whole content, re-measures every line
// (ANSI-aware widths) on each SetContent/AtBottom/View — O(transcript)
// per frame, which is what made long sessions lag. The scroller instead
// receives lines ALREADY wrapped at the content width: appends and
// paints cost O(new lines + visible window), independent of how much
// transcript sits above. Scroll state mirrors the viewport's: pinned to
// the bottom while following, absolute line offset once scrolled up.
type scroller struct {
	height  int
	lines   []string // committed wrapped lines (from blocks)
	live    []string // streaming tail (replaced wholesale per sync)
	yOffset int      // first visible line (valid when !pinned)
	pinned  bool     // follow the bottom as content grows
}

// SetHeight resizes the window (clamped to at least one line).
func (s *scroller) SetHeight(h int) {
	if h < 1 {
		h = 1
	}
	s.height = h
}

// Height returns the window height.
func (s *scroller) Height() int { return s.height }

func (s *scroller) total() int     { return len(s.lines) + len(s.live) }
func (s *scroller) maxUp() int     { return max(0, s.total()-s.height) }
func (s *scroller) AtBottom() bool { return s.pinned || s.yOffset >= s.maxUp() }

// firstVisible reports the absolute index of the first visible line —
// the same clamp view() applies, without mutating scroll state. Mouse
// hit-testing and selection highlighting must agree with what is
// painted, so both read through this.
func (s *scroller) firstVisible() int {
	if s.pinned || s.yOffset > s.maxUp() {
		return s.maxUp()
	}
	return s.yOffset
}

// lineAt indexes the combined transcript by absolute line number:
// committed lines first, then the live streaming tail.
func (s *scroller) lineAt(i int) (string, bool) {
	if i < 0 || i >= s.total() {
		return "", false
	}
	if i < len(s.lines) {
		return s.lines[i], true
	}
	return s.live[i-len(s.lines)], true
}

// GotoBottom re-pins the window to the end of the content.
func (s *scroller) GotoBottom() { s.pinned = true }

// appendLines commits already-wrapped lines — O(new).
func (s *scroller) appendLines(lines ...string) {
	s.lines = append(s.lines, lines...)
}

// replaceLines splices new lines over [start, start+oldCount) in place
// (tool status dots re-render their row once the call settles). Cost is
// proportional to the tail, and only fires on discrete tool events.
func (s *scroller) replaceLines(start, oldCount int, newLines []string) {
	if start < 0 || start+oldCount > len(s.lines) || oldCount < 0 {
		return // out of range: leave the stale row rather than corrupt the transcript
	}
	s.lines = slices.Replace(s.lines, start, start+oldCount, newLines...)
}

// setLive replaces the streaming tail (the per-frame wrapped deltas).
func (s *scroller) setLive(lines []string) { s.live = lines }

// setLines replaces the committed lines wholesale (re-wrap after a
// width change or thinking expand — rare, O(transcript) by design).
func (s *scroller) setLines(lines []string) {
	s.lines = lines
	if !s.pinned {
		s.yOffset = min(s.yOffset, s.maxUp())
	}
}

// scrollUp moves the window up n lines, leaving follow mode.
func (s *scroller) scrollUp(n int) {
	if s.pinned {
		s.yOffset = s.maxUp()
		s.pinned = false
	}
	s.yOffset = max(0, s.yOffset-n)
}

// scrollDown moves the window down n lines, re-pinning at the bottom.
func (s *scroller) scrollDown(n int) {
	if s.pinned {
		return
	}
	if s.yOffset+n >= s.maxUp() {
		s.pinned = true
		return
	}
	s.yOffset += n
}

func (s *scroller) pageUp()   { s.scrollUp(s.height) }
func (s *scroller) pageDown() { s.scrollDown(s.height) }

// view renders the visible window — O(window), never O(transcript).
// The window always fills the scroller's height (short transcripts pad
// with blank lines) so the composer below stays anchored to the bottom
// of the terminal window instead of riding up under the content.
func (s *scroller) view() string {
	if s.pinned || s.yOffset > s.maxUp() {
		s.yOffset = s.maxUp()
	}
	start := s.yOffset
	end := start + s.height
	var b strings.Builder
	for i := start; i < end; i++ {
		if i > start {
			b.WriteByte('\n')
		}
		switch {
		case i < len(s.lines):
			b.WriteString(s.lines[i])
		case i < len(s.lines)+len(s.live):
			b.WriteString(s.live[i-len(s.lines)])
		}
	}
	return b.String()
}

// GetContent joins committed + live lines (inspection helper for tests).
func (s *scroller) GetContent() string {
	return strings.Join(append(slices.Clone(s.lines), s.live...), "\n")
}

// wrapText splits text into lines hard-wrapped at width cells — the
// same visuals as the bubbles viewport's soft wrap (ANSI-safe cuts at
// exact cell boundaries), but computed once per block instead of once
// per frame.
func wrapText(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	src := strings.Split(s, "\n")
	out := make([]string, 0, len(src))
	for _, line := range src {
		out = append(out, wrapLine(line, width)...)
	}
	return out
}

// wrapLine passes short lines through and cuts over-long ones into
// width-cell segments (the viewport's ansi.Cut loop).
func wrapLine(line string, width int) []string {
	lw := ansi.StringWidth(line)
	if lw <= width {
		return []string{line}
	}
	var out []string
	for idx := 0; idx < lw; idx += width {
		out = append(out, ansi.Cut(line, idx, idx+width))
	}
	return out
}

// streamBuf accumulates one streaming block (assistant text or
// thinking). It lives behind a pointer so model value-copies share it
// (a bare strings.Builder panics when written through a copy). Text
// keeps an incremental wrap cache: every line a delta closes is wrapped
// immediately, so each frame re-wraps only the trailing partial line
// and a flush commits the cached lines with no re-layout.
type streamBuf struct {
	kind string // "text" | "thinking"
	b    strings.Builder

	done []string        // wrapped complete lines (text kind)
	part strings.Builder // trailing partial line (no '\n' yet)
}

func (s *streamBuf) text() string { return s.b.String() }

// appendText folds a text delta in: bytes accumulate, and lines the
// delta closes are wrapped once, right now.
func (s *streamBuf) appendText(d string, width int) {
	s.b.WriteString(d)
	s.wrapChunk(d, width)
}

func (s *streamBuf) wrapChunk(d string, width int) {
	for len(d) > 0 {
		i := strings.IndexByte(d, '\n')
		if i < 0 {
			s.part.WriteString(d)
			return
		}
		s.part.WriteString(d[:i])
		line := strings.TrimSuffix(s.part.String(), "\r") // \r\n deltas normalize
		s.part.Reset()
		s.done = append(s.done, wrapLine(line, width)...)
		d = d[i+1:]
	}
}

// resetWrap rebuilds the wrap cache from the raw text (width change).
func (s *streamBuf) resetWrap(width int) {
	s.done = nil
	s.part.Reset()
	s.wrapChunk(s.text(), width)
}

// viewLines returns the wrapped streaming lines: committed lines plus
// the freshly wrapped partial tail.
func (s *streamBuf) viewLines(width int) []string {
	out := make([]string, 0, len(s.done)+1)
	out = append(out, s.done...)
	return append(out, wrapLine(s.part.String(), width)...)
}
