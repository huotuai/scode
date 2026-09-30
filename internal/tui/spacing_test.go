package tui

import (
	"net/http"
	"strings"
	"testing"

	"scode/internal/agent"
	"scode/internal/llm"
)

// Blank lines separate every block KIND — text, tool rows, thinking —
// and a tool row settles in place without eating its leading spacer or
// leaving a stale start row behind (regression: the toolCalls index
// was recorded before ensureSpacer appended one, so EvToolEnd
// overwrote the spacer block and the dim start row survived).
func TestBlockKindSpacing(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	m := newModel(app, make(chan any, 4))
	m.width, m.height = 80, 24

	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextDelta, Delta: "第一段正文"}})
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextEnd}})
	call := llm.Block{Kind: llm.BlockToolCall, ID: "t1", Name: "read", Arguments: []byte(`{"path":"x.go"}`)}
	m.renderEvent(agent.Event{Type: agent.EvToolStart, Call: &call})
	res := agent.TextResult("ok")
	m.renderEvent(agent.Event{Type: agent.EvToolEnd, Call: &call, Result: &res})
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventThinkingDelta, Delta: "思考内容"}})
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventThinkingEnd}})
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextDelta, Delta: "第二段正文"}})
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextEnd}})
	m.flushLive()

	// Every content block is preceded by exactly one spacer block, and
	// spacers stay spacers (never rewritten into content).
	var contentIdx []int
	for i, b := range m.blocks {
		if i == 0 || b.spacer {
			continue // the welcome banner and spacer blocks
		}
		contentIdx = append(contentIdx, i)
	}
	for k, i := range contentIdx {
		if k == 0 {
			continue // the first content block follows the banner's spacer
		}
		prev := m.blocks[i-1]
		if !prev.spacer || len(prev.lines) != 1 || strings.TrimSpace(joinLines(prev.lines)) != "" {
			t.Fatalf("blocks[%d] (content) lacks a clean 1-line spacer before it: %+v", i, prev)
		}
	}

	// The tool row settled in place: the transcript carries exactly ONE
	// Read row — no stale dim duplicate — and never inside a spacer.
	view := ""
	for _, b := range m.blocks {
		view += joinLines(b.lines) + "\n"
	}
	if n := strings.Count(view, "Read"); n != 1 {
		t.Fatalf("Read row appears %d times (stale duplicate):\n%s", n, view)
	}
	for i, b := range m.blocks {
		if strings.Contains(joinLines(b.lines), "Read") && b.spacer {
			t.Fatalf("the Read row landed in a spacer block (%d)", i)
		}
	}

	// And the visible viewport has blank lines between the three kinds.
	vp := joinLines(m.vp.lines)
	for _, seg := range []string{"第一段正文", "Read", "思考内容", "第二段正文"} {
		if !strings.Contains(vp, seg) {
			t.Fatalf("%q missing from the viewport:\n%s", seg, vp)
		}
	}
}

func joinLines(ls []string) string { return strings.Join(ls, "\n") }

// The streaming tail carries its leading gap WHILE generating, not
// only after the flush commits the spacer (regression: text/thinking
// streams hugged the block above them mid-run, then everything jumped
// down one line when flushLive's ensureSpacer landed).
func TestStreamingTailLeadsWithGap(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	m := newModel(app, make(chan any, 4))
	m.width, m.height = 80, 24

	// Commit one block, then stream thinking: the live tail must open
	// with a blank line so the gap is visible mid-stream.
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextDelta, Delta: "第一段正文"}})
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextEnd}})
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventThinkingDelta, Delta: "思考中"}})
	m.syncTail()
	if len(m.vp.live) == 0 || strings.TrimSpace(plainText(m.vp.live[0])) != "" {
		t.Fatalf("streaming tail lacks its leading gap: %#v", m.vp.live)
	}
	before := m.vp.total()

	// Flushing swaps the live gap for the committed spacer one-for-one:
	// the transcript does not shift.
	m.flushLive()
	m.syncTail()
	if m.vp.total() != before {
		t.Fatalf("flush shifted the transcript: %d -> %d lines", before, m.vp.total())
	}

	// Streaming text after a committed spacer (already gapped) must not
	// double the gap.
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextDelta, Delta: "第二段正文"}})
	m.syncTail()
	if len(m.vp.live) == 0 || m.vp.live[0] != "" {
		t.Fatalf("text tail lacks its leading gap: %#v", m.vp.live)
	}
	if len(m.vp.live) > 1 && m.vp.live[1] == "" {
		t.Fatalf("doubled gap above the streaming tail: %#v", m.vp.live)
	}
}
