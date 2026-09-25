package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"scode/internal/llm"
	"scode/internal/session"
)

// Compact summarizes a conversation into a checkpoint. The summary call
// is a one-off: no tools, and caching disabled (CacheNone) so it neither
// pays cache writes nor evicts the main conversation's cache — pi's
// compaction economics. Returns the summary and the merged file working
// set for the next compaction generation.
func (a *Agent) Compact(ctx context.Context, t *llm.Transcript, prev session.FileOps) (string, session.FileOps, error) {
	msgs := t.Messages()

	sumTr, err := llm.NormalizeContext(llm.Context{
		SystemPrompt: session.SummarizationSystemPrompt,
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock(session.BuildSummaryPrompt(msgs, prev))}, TS: time.Now().UnixMilli()},
		},
	})
	if err != nil {
		return "", prev, err
	}

	opts := a.cfg.Stream
	opts.Cache = llm.CacheNone // one-off call: do not touch the main cache
	opts.MaxTokens = 4096
	if opts.ThinkingLevel != "" {
		opts.ThinkingLevel = "off" // summaries need no reasoning
	}

	events, err := a.cfg.Provider.Stream(ctx, a.cfg.Model, sumTr, opts)
	if err != nil {
		return "", prev, err
	}
	var final *llm.Message
	for ev := range events {
		if ev.Terminal() {
			final = ev.Message
		}
	}
	if final == nil || final.StopReason == llm.StopError || final.StopReason == llm.StopLength {
		// Length-stopped summaries are truncated text — persisting one
		// as the checkpoint would silently discard context (pi rejects
		// them too).
		if final != nil {
			return "", prev, fmt.Errorf("summarization failed: %s (%s)", final.Error, final.StopReason)
		}
		return "", prev, fmt.Errorf("summarization stream ended without a terminal event")
	}

	var sb strings.Builder
	for _, b := range final.Content {
		if b.Kind == llm.BlockText {
			sb.WriteString(b.Text)
		}
	}
	if strings.TrimSpace(sb.String()) == "" {
		return "", prev, fmt.Errorf("summarization produced no text")
	}
	return sb.String(), session.ExtractFileOps(msgs, prev), nil
}
