// Package tools implements scode's built-in coding tools: read, write,
// edit, bash, grep, find, ls. Schemas, truncation limits, and error
// phrasing follow pi's battle-tested values (packages/coding-agent/src/
// core/tools) so models trained on common agent conventions behave.
package tools

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Truncation limits (pi's truncate.ts defaults).
const (
	MaxLines       = 2000      // per read/grep/ls/find result
	MaxBytes       = 50 * 1024 // 50 KiB per tool result
	GrepMaxLineLen = 500       // per grep match line
)

// TruncationResult carries the truncated text plus the continuation
// notice models use to page further.
type TruncationResult struct {
	Text      string
	Truncated bool
	// Notice is appended to Text when truncated; it tells the model how to
	// continue (offset for files) or that output was cut.
	Notice string
}

// TruncateHead keeps the first complete lines within both limits (files:
// the interesting header comes first). A single line over the byte budget
// yields an empty result with an explanatory notice.
func TruncateHead(s string) TruncationResult {
	return truncate(s, false)
}

// TruncateTail keeps the last complete lines (shell output: errors sit at
// the end). If the final line alone exceeds the byte budget it is cut
// mid-line from the end, UTF-8-safely.
func TruncateTail(s string) TruncationResult {
	return truncate(s, true)
}

func truncate(s string, fromEnd bool) TruncationResult {
	if s == "" {
		return TruncationResult{}
	}
	lines := strings.SplitAfter(s, "\n")
	n := len(lines)
	total := 0
	for _, l := range lines {
		total += len(l)
	}
	if n <= MaxLines && total <= MaxBytes {
		return TruncationResult{Text: s}
	}

	res := TruncationResult{Truncated: true}
	var kept []string
	bytes := 0
	if !fromEnd {
		for i, l := range lines {
			if i == 0 && len(l) > MaxBytes {
				res.Notice = fmt.Sprintf("[First line exceeds %d bytes; use bash to extract a slice, e.g. sed -n '1p' FILE | head -c %d]", MaxBytes, MaxBytes)
				return res
			}
			if i >= MaxLines || bytes+len(l) > MaxBytes {
				break
			}
			kept = append(kept, l)
			bytes += len(l)
		}
		shown := len(kept)
		res.Text = strings.Join(kept, "")
		if !strings.HasSuffix(res.Text, "\n") && shown > 0 && shown < n {
			res.Text += "\n"
		}
		res.Notice = fmt.Sprintf("[Showing lines 1-%d of %d; further content omitted]", shown, n-1)
	} else {
		for i := n - 1; i >= 0; i-- {
			l := lines[i]
			if len(kept) >= MaxLines {
				break
			}
			if bytes+len(l) > MaxBytes {
				if len(kept) == 0 {
					// Sole survivor line too big: cut from the end.
					res.Text = truncateFromEndUTF8(l, MaxBytes)
					res.Notice = fmt.Sprintf("[Showing last %d bytes of a %d-byte final line]", len(res.Text), len(l))
					return res
				}
				break
			}
			kept = append([]string{l}, kept...)
			bytes += len(l)
		}
		shown := len(kept)
		res.Text = strings.Join(kept, "")
		res.Notice = fmt.Sprintf("[Showing last %d lines; earlier output omitted]", shown)
	}
	return res
}

// truncateFromEndUTF8 keeps the last n bytes without splitting a rune.
func truncateFromEndUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return s[cut:]
}

// ClampLine shortens one match line for grep output.
func ClampLine(s string) string {
	if len(s) <= GrepMaxLineLen {
		return s
	}
	return s[:GrepMaxLineLen] + "…"
}
