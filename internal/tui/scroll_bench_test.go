package tui

import (
	"fmt"
	"strings"
	"testing"

	"scode/internal/agent"
	"scode/internal/llm"
)

// BenchmarkStreamFrameLargeTranscript measures one streaming frame (a
// flush tick: syncViewport + view) against a long-session transcript.
// The scroller keeps this O(new content + window) — the number must not
// grow with the transcript size (the old full-SetContent viewport was
// O(transcript) per frame, which is what made long sessions lag).
func BenchmarkStreamFrameLargeTranscript(b *testing.B) {
	for _, blocks := range []int{100, 2000, 20000} {
		b.Run(fmt.Sprintf("blocks=%d", blocks), func(b *testing.B) {
			m := newTestModel()
			m.vp.SetHeight(24)
			for i := range blocks {
				m.appendBlock(fmt.Sprintf("line %d: %s", i, strings.Repeat("x", 40)))
			}
			m.beginLive("text")
			m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{
				Type: llm.EventTextDelta, Delta: strings.Repeat("streaming text ", 32),
			}})
			b.ResetTimer()
			for range b.N {
				m.dirty = true
				m.syncViewport()
				_ = m.vp.view()
			}
		})
	}
}

// BenchmarkDeltaIngest measures per-delta accumulation cost: a streamBuf
// append must be O(delta), not O(buffered text so far).
func BenchmarkDeltaIngest(b *testing.B) {
	m := newTestModel()
	m.beginLive("text")
	const delta = "hello streaming world "
	b.ResetTimer()
	for range b.N {
		m.live.appendText(delta, m.width)
	}
}

// BenchmarkThinkingFrame measures the per-frame live-thinking render
// with a large accumulated buffer — the tail cap must bound it.
func BenchmarkThinkingFrame(b *testing.B) {
	for _, kb := range []int{4, 64, 512} {
		b.Run(fmt.Sprintf("buffer=%dkb", kb), func(b *testing.B) {
			m := newTestModel()
			m.beginLive("thinking")
			m.live.b.WriteString(strings.Repeat("thought line of reasoning here.\n", kb*1024/32))
			b.ResetTimer()
			for range b.N {
				_ = m.liveViewLines()
			}
		})
	}
}
