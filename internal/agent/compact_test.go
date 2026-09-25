package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"scode/internal/llm"
	"scode/internal/session"
)

// optsProvider scripts one response and records the request transcript
// and stream options it received.
type optsProvider struct {
	mu       sync.Mutex
	reply    llm.Message
	gotOpts  []llm.StreamOptions
	gotTrans []*llm.Transcript
}

func (p *optsProvider) Name() string { return "opts" }
func (p *optsProvider) Caps() llm.Capabilities {
	return llm.Capabilities{}
}

func (p *optsProvider) Stream(_ context.Context, _ llm.Model, t *llm.Transcript, o llm.StreamOptions) (<-chan llm.Event, error) {
	p.mu.Lock()
	p.gotOpts = append(p.gotOpts, o)
	cp := &llm.Transcript{}
	for _, m := range t.Messages() {
		_ = cp.Append(m)
	}
	p.gotTrans = append(p.gotTrans, cp)
	reply := p.reply
	p.mu.Unlock()

	out := make(chan llm.Event, 4)
	go func() {
		defer close(out)
		m := reply
		out <- llm.Event{Type: llm.EventStart, Message: &m}
		f := reply
		out <- llm.Event{Type: llm.EventDone, Message: &f, Reason: reply.StopReason}
	}()
	return out, nil
}

func compactTranscript(t *testing.T) *llm.Transcript {
	t.Helper()
	tr, err := llm.NormalizeContext(llm.Context{
		SystemPrompt: "agent prompt",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("fix the login bug")}, TS: 1},
			{
				Role: llm.RoleAssistant, StopReason: llm.StopToolUse, TS: 2,
				Content: []llm.Block{{Kind: llm.BlockToolCall, ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"auth.go"}`)}},
			},
			{
				Role: llm.RoleTool, TS: 3,
				Content: []llm.Block{{Kind: llm.BlockToolResult, ID: "c1", Content: []llm.Block{llm.TextBlock("func login() {}")}}},
			},
			{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("fixed")}, TS: 4},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestCompactSummarizesWithCacheNone(t *testing.T) {
	p := &optsProvider{reply: llm.Message{
		Role: llm.RoleAssistant, StopReason: llm.StopEndTurn,
		Content: []llm.Block{llm.TextBlock("Task: fix login bug. State: fixed and verified.")},
	}}
	a := New(Config{Provider: p, Model: llm.Model{ID: "m"}, Tools: NewRegistry()})

	summary, ops, err := a.Compact(context.Background(), compactTranscript(t), session.FileOps{Read: []string{"old.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "fix login bug") {
		t.Fatalf("summary = %q", summary)
	}
	if len(ops.Read) != 2 || ops.Read[0] != "auth.go" || ops.Read[1] != "old.go" {
		t.Fatalf("file ops = %+v", ops)
	}

	// The one-off call: caching disabled, no tools declared.
	if len(p.gotOpts) != 1 {
		t.Fatalf("calls = %d", len(p.gotOpts))
	}
	if p.gotOpts[0].Cache != llm.CacheNone {
		t.Fatalf("cache = %q, want none", p.gotOpts[0].Cache)
	}
	msgs := p.gotTrans[0].Messages()
	if len(msgs) < 2 || msgs[0].Role != llm.RoleSystem || len(llm.CurrentTools(msgs)) != 0 {
		t.Fatalf("summarization transcript shape wrong: %+v", msgs)
	}
	if !strings.Contains(msgs[1].Content[0].Text, "fix the login bug") {
		t.Fatal("summary prompt missing the conversation")
	}
	if !strings.Contains(msgs[0].Content[0].Text, "summarize") {
		t.Fatal("summarization system prompt missing")
	}
}

func TestCompactProviderErrorPropagates(t *testing.T) {
	p := &optsProvider{reply: llm.Message{Role: llm.RoleAssistant, StopReason: llm.StopError, Error: "quota"}}
	a := New(Config{Provider: p, Model: llm.Model{ID: "m"}})
	if _, _, err := a.Compact(context.Background(), compactTranscript(t), session.FileOps{}); err == nil {
		t.Fatal("provider error must propagate")
	}
}

func TestCompactEmptySummaryRejected(t *testing.T) {
	p := &optsProvider{reply: llm.Message{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("   ")}}}
	a := New(Config{Provider: p, Model: llm.Model{ID: "m"}})
	if _, _, err := a.Compact(context.Background(), compactTranscript(t), session.FileOps{}); err == nil {
		t.Fatal("empty summary must be rejected")
	}
}
