package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"scode/internal/llm"
	"scode/internal/session"
)

// Compact summarizes msgs into a checkpoint. prefix is the full
// conversation context at the cut point (folded system + projected
// messages); on auto-caching providers the summary call reuses it
// verbatim (dsh's compaction: the one-shot stream carries the
// conversation's own prefix, so the provider KV cache serves the bulk
// of the request). Explicit-breakpoint providers (Anthropic) keep pi's
// economics: a fresh serialized call with caching disabled (CacheNone),
// never touching the main conversation's cache. A previous summary
// (prevSummary) switches the legacy call into pi's update flow: merge
// instead of re-summarize — with prefix reuse the previous summary is
// already in context (the projection injects it as a user message), so
// the generic instruction suffices. custom carries optional
// user-supplied focus instructions (manual /compact). Returns the
// summary and the merged file working set for the next compaction
// generation.
func (a *Agent) Compact(ctx context.Context, msgs, prefix []llm.Message, prevSummary, custom string, prev session.FileOps) (string, session.FileOps, error) {
	instruction := ""
	if prevSummary != "" {
		instruction = "<previous-summary>\n" + prevSummary + "\n</previous-summary>\n\n" + session.UpdateSummarizationPrompt
	} else {
		instruction = session.SummarizationPrompt
	}
	if custom != "" {
		instruction += "\n\nAdditional instructions from the user:\n" + custom
	}
	var summary string
	var err error
	if a.cfg.Model.Caps.AutoCache && len(prefix) > 0 {
		summary, err = a.prefixSummary(ctx, prefix, instruction)
	} else {
		user := session.BuildSummaryPrompt(msgs, prev) + "\n" + instruction
		summary, err = a.oneOffSummary(ctx, user)
	}
	if err != nil {
		return "", prev, err
	}
	return summary, session.ExtractFileOps(msgs, prev), nil
}

// CompactTurnPrefix summarizes the partial turn preceding a mid-turn
// cut point (pi's generateTurnPrefixSummary).
func (a *Agent) CompactTurnPrefix(ctx context.Context, msgs, prefix []llm.Message) (string, error) {
	if a.cfg.Model.Caps.AutoCache && len(prefix) > 0 {
		return a.prefixSummary(ctx, prefix, session.TurnPrefixSummarizationPrompt)
	}
	user := session.BuildSummaryPrompt(msgs, session.FileOps{}) + "\n" + session.TurnPrefixSummarizationPrompt
	return a.oneOffSummary(ctx, user)
}

// prefixSummary runs the summarization call on top of the conversation's
// own context: the instruction rides as the final user message, so the
// request shares a byte prefix with the main conversation's requests and
// auto-caching providers serve it from KV (dsh's summarize()).
func (a *Agent) prefixSummary(ctx context.Context, prefix []llm.Message, instruction string) (string, error) {
	sumTr, err := llm.NewTranscript(prefix[0], append(prefix[1:], llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.Block{llm.TextBlock(instruction)},
		TS:      time.Now().UnixMilli(),
	})...)
	if err != nil {
		return "", err
	}
	return a.streamSummary(ctx, sumTr, false)
}

// oneOffSummary runs a single cache-disabled, tool-less summarization
// call (pi's compaction economics).
func (a *Agent) oneOffSummary(ctx context.Context, user string) (string, error) {
	sumTr, err := llm.NormalizeContext(llm.Context{
		SystemPrompt: session.SummarizationSystemPrompt,
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock(user)}, TS: time.Now().UnixMilli()},
		},
	})
	if err != nil {
		return "", err
	}
	return a.streamSummary(ctx, sumTr, true)
}

// streamSummary drives one summarization request; cacheNone selects pi's
// CacheNone economics (explicit-breakpoint providers).
func (a *Agent) streamSummary(ctx context.Context, sumTr *llm.Transcript, cacheNone bool) (string, error) {
	opts := a.cfg.Stream
	if cacheNone {
		opts.Cache = llm.CacheNone // one-off call: do not touch the main cache
	}
	opts.MaxTokens = 4096
	if opts.ThinkingLevel != "" {
		opts.ThinkingLevel = "off" // summaries need no reasoning
	}
	events, err := a.cfg.Provider.Stream(ctx, a.cfg.Model, sumTr, opts)
	if err != nil {
		return "", err
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
			return "", fmt.Errorf("summarization failed: %s (%s)", final.Error, final.StopReason)
		}
		return "", fmt.Errorf("summarization stream ended without a terminal event")
	}

	var sb strings.Builder
	for _, b := range final.Content {
		if b.Kind == llm.BlockText {
			sb.WriteString(b.Text)
		}
	}
	if strings.TrimSpace(sb.String()) == "" {
		return "", fmt.Errorf("summarization produced no text")
	}
	return sb.String(), nil
}
