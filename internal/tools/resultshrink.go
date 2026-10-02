package tools

import (
	"fmt"
	"strings"

	"scode/internal/agent"
	"scode/internal/llm"
)

// resultshrink.go is the agent-layer After hook that middle-elides
// oversized tool results BEFORE they enter the transcript (the tools'
// own 2000-line/50KB caps alone let three broad queries eat half of
// the default 80K-token compaction budget). The shrunk text is what
// gets persisted to the session JSONL, so the transcript — and the
// provider prefix cache — stays stable: history is never rewritten.
//
// Design notes:
//   - One-shot, deterministic, idempotent: the marker contains no data
//     the model could need later, and re-running on an already-shrunk
//     result is a no-op (below threshold after elision).
//   - Head-biased: grep/read results are most informative at the top;
//     the tail keeps tool notices ([Showing lines X-Y of N], spill
//     paths) that tools append at the end.

const (
	// ShrinkThresholdBytes triggers middle elision (~6K tokens).
	ShrinkThresholdBytes = 24 * 1024
	// shrinkHeadBytes / shrinkTailBytes bound the kept regions.
	shrinkHeadBytes = 16 * 1024
	shrinkTailBytes = 4 * 1024
	// shrinkHeadLines / shrinkTailLines cap line counts so many short
	// lines cannot re-inflate the kept regions.
	shrinkHeadLines = 300
	shrinkTailLines = 60
)

// ShrinkResultMarker opens the elision notice. Stable string: tests
// and any future "already shrunk?" detection key on it.
const ShrinkResultMarker = "[… omitted by scode to keep context lean:"

// ShrinkOversizedResult implements agent.AfterToolCall. It treats the
// input as immutable and returns a new result; non-text blocks and
// error results pass through untouched.
func ShrinkOversizedResult(call llm.Block, res agent.ToolResult) agent.ToolResult {
	if res.IsError {
		return res
	}
	changed := false
	out := agent.ToolResult{IsError: res.IsError, Content: make([]llm.Block, len(res.Content))}
	for i, b := range res.Content {
		out.Content[i] = b
		if b.Kind != llm.BlockText || len(b.Text) <= ShrinkThresholdBytes {
			continue
		}
		if shrunk := middleElide(b.Text); shrunk != b.Text {
			out.Content[i].Text = shrunk
			changed = true
		}
	}
	if !changed {
		return res
	}
	return out
}

// middleElide keeps the head and tail of s with a marker between.
func middleElide(s string) string {
	lines := strings.Split(s, "\n")

	// Few but huge lines (minified bundles, one-line JSON): byte-level
	// elision, since line-based keeping would retain everything.
	if len(lines) < shrinkHeadLines+shrinkTailLines+8 {
		if len(s) <= shrinkHeadBytes+shrinkTailBytes+1024 {
			return s
		}
		return s[:shrinkHeadBytes] +
			fmt.Sprintf("\n%s %d bytes of %d — re-run with a narrower query to see a specific section]\n",
				ShrinkResultMarker, len(s)-shrinkHeadBytes-shrinkTailBytes, len(s)) +
			s[len(s)-shrinkTailBytes:]
	}

	headBytes := 0
	head := 0
	for head < len(lines) && head < shrinkHeadLines {
		if headBytes+len(lines[head]) > shrinkHeadBytes {
			break
		}
		headBytes += len(lines[head]) + 1
		head++
	}
	tailBytes := 0
	tail := 0
	for tail < len(lines)-head && tail < shrinkTailLines {
		l := lines[len(lines)-1-tail]
		if tailBytes+len(l) > shrinkTailBytes {
			break
		}
		tailBytes += len(l) + 1
		tail++
	}
	omitted := len(lines) - head - tail
	if omitted <= 8 {
		return s // elision would save almost nothing
	}
	var b strings.Builder
	b.Grow(headBytes + tailBytes + 160)
	b.WriteString(strings.Join(lines[:head], "\n"))
	b.WriteString(fmt.Sprintf("\n%s %d lines of %d — re-run with a narrower pattern/path or a smaller read window to see a specific section]\n",
		ShrinkResultMarker, omitted, len(lines)))
	b.WriteString(strings.Join(lines[len(lines)-tail:], "\n"))
	return b.String()
}
