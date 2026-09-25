package tools

import (
	"fmt"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Edit matching follows pi's edit-diff.ts algorithm:
//
//  1. Content is normalized to LF (BOM stripped, CRLF/CR folded); the
//     detected original line ending is restored on write.
//  2. Each edit's oldText is matched against the ORIGINAL file content,
//     not the output of earlier edits; overlapping matches are rejected.
//  3. Matching tries exact indexOf first, then a fuzzy pass: both sides
//     pass through normalizeForFuzzyMatch (NFKC, trailing-whitespace
//     trim, smart quotes -> ASCII, unicode dashes -> '-').
//  4. oldText must match exactly once — "provide more context" otherwise.
//
// Coordinate spaces: exact matches locate offsets in the original
// content. Fuzzy matches locate offsets in normalized content and are
// immediately converted to original-space bounds by expanding to whole
// lines (pi's applyReplacementsPreservingUnchangedLines, simplified:
// the partial first/last line segments outside the match are preserved
// from the original bytes). All downstream logic (overlap, replacement)
// then works in original space.

type lineEnding string

const (
	leLF   lineEnding = "\n"
	leCRLF lineEnding = "\r\n"
)

func detectLineEnding(s string) lineEnding {
	if strings.Contains(s, "\r\n") {
		return leCRLF
	}
	return leLF
}

func normalizeToLF(s string) string {
	s = strings.TrimPrefix(s, "\uFEFF")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

func restoreLineEndings(s string, le lineEnding) string {
	if le == leLF {
		return s
	}
	return strings.ReplaceAll(s, "\n", "\r\n")
}

// normalizeForFuzzyMatch relaxes text for the fuzzy pass.
func normalizeForFuzzyMatch(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\u2018', '\u2019', '\u201A', '\u201B': // ‘ ’ ‚ ‛
			b.WriteByte('\'')
		case '\u201C', '\u201D', '\u201E', '\u201F': // “ ” „ ‟
			b.WriteByte('"')
		case '\u2010', '\u2011', '\u2012', '\u2013', '\u2014', '\u2212': // – — −
			b.WriteByte('-')
		case '\u00A0', '\u2007', '\u202F': // nbsp variants
			b.WriteByte(' ')
		default:
			b.WriteString(norm.NFKC.String(string(r)))
		}
	}
	lines := strings.Split(b.String(), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.Join(lines, "\n")
}

type editRequest struct {
	oldText string
	newText string
}

// locatedEdit is one edit with original-space replacement bounds.
type locatedEdit struct {
	edit  editRequest
	fuzzy bool
	start int // byte offset into the LF-normalized original content
	end   int
}

// findExact locates a unique exact occurrence.
func findExact(content, oldText string) (int, int, bool, error) {
	idx := strings.Index(content, oldText)
	if idx < 0 {
		return 0, 0, false, nil
	}
	if strings.Contains(content[idx+1:], oldText) {
		return 0, 0, false, fmt.Errorf("oldText matches more than once — provide more surrounding context to make it unique")
	}
	return idx, idx + len(oldText), true, nil
}

// lineMap converts offsets in normalized space to (line, offset-in-line)
// using the original lines' normalized forms.
type lineMap struct {
	origLines []string
	normLines []string
	starts    []int // normalized-space start offset of each line
}

func buildLineMap(content string) *lineMap {
	lm := &lineMap{origLines: strings.Split(content, "\n")}
	pos := 0
	for _, l := range lm.origLines {
		n := normalizeForFuzzyMatch(l)
		lm.normLines = append(lm.normLines, n)
		lm.starts = append(lm.starts, pos)
		pos += len(n) + 1 // +1 newline
	}
	return lm
}

// locate converts a normalized-space span to original-space bounds,
// expanding to whole-line granularity while preserving the untouched
// prefix of the first line and suffix of the last line.
func (lm *lineMap) locate(spanStart, spanEnd int) (int, int) {
	startLine, startOff := lm.lineOf(spanStart)
	endLine, endOff := lm.lineOf(spanEnd)

	// Original-space prefix: bytes of startLine before the match. The
	// normalized offset equals the original offset unless normalization
	// changed earlier bytes of the line (accepted rarity, documented).
	prefixLen := clampInt(startOff, len(lm.origLines[startLine]))
	start := lm.origStart(startLine) + prefixLen

	// Original-space suffix of endLine after the match.
	suffix := ""
	if endLine < len(lm.origLines) {
		off := clampInt(endOff, len(lm.origLines[endLine]))
		suffix = lm.origLines[endLine][off:]
	}
	end := lm.origStart(endLine) + len(lm.origLines[endLine]) - len(suffix)
	return start, end
}

func clampInt(n, max int) int {
	if n > max {
		return max
	}
	return n
}

func (lm *lineMap) lineOf(normOffset int) (line, off int) {
	for i := len(lm.starts) - 1; i >= 0; i-- {
		if normOffset >= lm.starts[i] {
			return i, normOffset - lm.starts[i]
		}
	}
	return 0, 0
}

// origStart is the original-space start offset of a line.
func (lm *lineMap) origStart(line int) int {
	n := 0
	for i := 0; i < line; i++ {
		n += len(lm.origLines[i]) + 1
	}
	return n
}

// findFuzzy locates a unique fuzzy occurrence and converts it to
// original-space bounds.
func findFuzzy(content, oldText string) (start, end int, found bool, err error) {
	lm := buildLineMap(content)
	var norm strings.Builder
	for i, l := range lm.normLines {
		if i > 0 {
			norm.WriteByte('\n')
		}
		norm.WriteString(l)
	}
	nContent := norm.String()
	nOld := normalizeForFuzzyMatch(oldText)

	var hits []int
	for i := 0; i+len(nOld) <= len(nContent); {
		if idx := strings.Index(nContent[i:], nOld); idx >= 0 {
			hits = append(hits, i+idx)
			i += idx + 1
		} else {
			break
		}
	}
	switch len(hits) {
	case 0:
		return 0, 0, false, nil
	case 1:
		s, e := lm.locate(hits[0], hits[0]+len(nOld))
		return s, e, true, nil
	default:
		return 0, 0, false, fmt.Errorf("oldText matches %d times after fuzzy normalization — provide more surrounding context", len(hits))
	}
}

// applyEdits validates and applies all edits against the original
// LF-normalized content. Overlap check runs on sorted positions;
// replacements apply in reverse offset order so earlier offsets stay
// valid. A no-op result is an error, matching pi.
func applyEdits(content string, edits []editRequest) (string, error) {
	var located []locatedEdit
	for _, e := range edits {
		if e.oldText == "" {
			return "", fmt.Errorf("oldText must not be empty")
		}
		if strings.TrimSpace(content) == "" && strings.TrimSpace(e.oldText) != "" {
			return "", fmt.Errorf("file is empty; use the write tool to create content")
		}
		start, end, found, err := findExact(content, e.oldText)
		fuzzy := false
		if !found && err == nil {
			start, end, found, err = findFuzzy(content, e.oldText)
			fuzzy = found
		}
		if err != nil {
			return "", err
		}
		if !found {
			return "", fmt.Errorf("oldText not found in file (after exact and fuzzy matching)")
		}
		located = append(located, locatedEdit{edit: e, fuzzy: fuzzy, start: start, end: end})
	}

	sort.Slice(located, func(a, b int) bool { return located[a].start < located[b].start })
	for k := 1; k < len(located); k++ {
		if located[k].start < located[k-1].end {
			return "", fmt.Errorf("edits overlap: replacement windows [%d,%d) and [%d,%d) intersect",
				located[k-1].start, located[k-1].end, located[k].start, located[k].end)
		}
	}

	out := content
	for i := len(located) - 1; i >= 0; i-- {
		a := located[i]
		out = out[:a.start] + a.edit.newText + out[a.end:]
	}
	if out == content {
		return "", fmt.Errorf("no changes made: new content is identical")
	}
	return out, nil
}
