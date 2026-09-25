package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"scode/internal/llm"
)

// scriptedProvider replays a queue of canned assistant messages, one per
// Stream call — the fake that drives loop tests.
type scriptedProvider struct {
	mu      sync.Mutex
	calls   int
	script  []llm.Message
	streams []*llm.Transcript // what the loop sent, per call
}

func (p *scriptedProvider) Name() string { return "scripted" }
func (p *scriptedProvider) Caps() llm.Capabilities {
	return llm.Capabilities{CacheBreakpoints: false}
}

func (p *scriptedProvider) Stream(_ context.Context, _ llm.Model, t *llm.Transcript, _ llm.StreamOptions) (<-chan llm.Event, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := &llm.Transcript{}
	for _, m := range t.Messages() {
		_ = cp.Append(m)
	}
	p.streams = append(p.streams, cp)
	i := p.calls
	p.calls++
	if i >= len(p.script) {
		return nil, fmt.Errorf("script exhausted")
	}
	msg := p.script[i]
	out := make(chan llm.Event, 8)
	go func() {
		defer close(out)
		m := msg
		out <- llm.Event{Type: llm.EventStart, Message: &m}
		for _, b := range msg.Content {
			if b.Kind == llm.BlockText {
				out <- llm.Event{Type: llm.EventTextDelta, Delta: b.Text}
			}
		}
		if msg.StopReason == llm.StopError {
			e := msg
			out <- llm.Event{Type: llm.EventError, Message: &e, Reason: llm.StopError, Err: errors.New(msg.Error)}
		} else {
			f := msg
			out <- llm.Event{Type: llm.EventDone, Message: &f, Reason: msg.StopReason}
		}
	}()
	return out, nil
}

// echoTool echoes its arguments; records execution order.
type echoTool struct {
	mu    sync.Mutex
	calls []string
	delay time.Duration
}

func (e *echoTool) Decl() llm.Tool {
	return llm.Tool{
		Name:        "echo",
		Description: "echo text",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
	}
}

func (e *echoTool) Execute(tc ToolContext, args json.RawMessage) ToolResult {
	var a struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return ErrorResult("bad args: " + err.Error())
	}
	if e.delay > 0 {
		select {
		case <-time.After(e.delay):
		case <-tc.Ctx.Done():
			return ErrorResult("cancelled")
		}
	}
	e.mu.Lock()
	e.calls = append(e.calls, a.Text)
	e.mu.Unlock()
	return TextResult("echo: " + a.Text)
}

func newTestAgent(p llm.Provider, tools ...Tool) *Agent {
	return New(Config{
		Provider: p,
		Model:    llm.Model{ID: "test-model", Provider: "scripted"},
		Tools:    NewRegistry(tools...),
	})
}

func collect(t *testing.T, a *Agent, tr *llm.Transcript, prompt string) ([]Event, error) {
	t.Helper()
	out := make(chan Event, 256)
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { done <- a.Prompt(ctx, tr, prompt, out) }()
	var events []Event
	for ev := range out {
		events = append(events, ev)
	}
	return events, <-done
}

func TestLoopToolRoundTrip(t *testing.T) {
	p := &scriptedProvider{script: []llm.Message{
		{
			Role: llm.RoleAssistant, StopReason: llm.StopToolUse,
			Content: []llm.Block{
				{Kind: llm.BlockToolCall, ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"text":"hi"}`)},
			},
		},
		{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("done: hi")}},
	}}
	echo := &echoTool{}
	a := newTestAgent(p, echo)
	tr, err := a.NewSession("test")
	if err != nil {
		t.Fatal(err)
	}
	events, runErr := collect(t, a, tr, "say hi via echo")
	if runErr != nil {
		t.Fatal(runErr)
	}

	// Event shape: agent_start, turn, llm..., assistant, tool_start/end, turn_end, ..., agent_end
	var kinds []string
	for _, ev := range events {
		kinds = append(kinds, string(ev.Type))
	}
	joined := strings.Join(kinds, " ")
	for _, want := range []string{"agent_start", "tool_start", "tool_end", "assistant", "agent_end"} {
		if !strings.Contains(joined, string(want)) {
			t.Fatalf("missing %s in %v", want, kinds)
		}
	}

	// The second provider call must contain the tool result (loop fed back).
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.streams) != 2 {
		t.Fatalf("provider called %d times, want 2", len(p.streams))
	}
	second := p.streams[1].Messages()
	var resultText string
	for _, m := range second {
		for _, b := range m.Content {
			if b.Kind == llm.BlockToolResult {
				for _, nb := range b.Content {
					resultText += nb.Text
				}
			}
		}
	}
	if resultText != "echo: hi" {
		t.Fatalf("tool result fed back = %q", resultText)
	}

	// Transcript final state.
	final := tr.Messages()
	if len(final) != 5 { // system, user, assistant(tool), tool, assistant(text)
		t.Fatalf("transcript len = %d", len(final))
	}
}

func TestLoopProviderErrorRecorded(t *testing.T) {
	// Turn-level retries replay the same turn, so the script needs one
	// failure per attempt before the run gives up.
	p := &scriptedProvider{script: []llm.Message{
		{Role: llm.RoleAssistant, StopReason: llm.StopError, Error: "rate limited"},
		{Role: llm.RoleAssistant, StopReason: llm.StopError, Error: "rate limited"},
		{Role: llm.RoleAssistant, StopReason: llm.StopError, Error: "rate limited"},
	}}
	a := newTestAgent(p, &echoTool{})
	tr, _ := a.NewSession("t")
	events, runErr := collect(t, a, tr, "go")
	if runErr == nil || !strings.Contains(runErr.Error(), "rate limited") {
		t.Fatalf("runErr = %v", runErr)
	}
	var sawErrEvent bool
	for _, ev := range events {
		if ev.Type == EvAgentError {
			sawErrEvent = true
		}
	}
	if !sawErrEvent {
		t.Fatal("no agent_error event")
	}
	// One replayable failure turn: exactly one error assistant message,
	// carrying text content so request builders can replay it.
	msgs := tr.Messages()
	errMsgs := 0
	for _, m := range msgs {
		if m.Role == llm.RoleAssistant && m.StopReason == llm.StopError {
			errMsgs++
			if len(m.Content) == 0 || m.Content[0].Kind != llm.BlockText {
				t.Fatalf("error turn lacks replayable content: %+v", m)
			}
		}
	}
	if errMsgs != 1 {
		t.Fatalf("error assistant messages = %d, want 1 (single encoding)", errMsgs)
	}
}

// Regression: a failed run must not poison the session — the next prompt
// still builds valid requests and succeeds.
func TestLoopSessionRecoversAfterFailure(t *testing.T) {
	p := &scriptedProvider{script: []llm.Message{
		{Role: llm.RoleAssistant, StopReason: llm.StopError, Error: "transient outage"},
		{Role: llm.RoleAssistant, StopReason: llm.StopError, Error: "transient outage"},
		{Role: llm.RoleAssistant, StopReason: llm.StopError, Error: "transient outage"},
		{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("back online")}},
	}}
	a := newTestAgent(p, &echoTool{})
	tr, _ := a.NewSession("t")

	if _, runErr := collect(t, a, tr, "try"); runErr == nil {
		t.Fatal("first run must fail")
	}
	// The provider the second run talks to must accept the transcript —
	// scriptedProvider replays it through Append, which would reject a
	// poisoned shape.
	events, runErr := collect(t, a, tr, "retry")
	if runErr != nil {
		t.Fatalf("session did not recover: %v", runErr)
	}
	var final string
	for _, ev := range events {
		if ev.Type == EvAssistant && ev.Message != nil {
			final = ev.Message.Content[0].Text
		}
	}
	if final != "back online" {
		t.Fatalf("final = %q", final)
	}
}

func TestLoopProviderErrorRetried(t *testing.T) {
	// A transient failure followed by success: only the successful turn
	// lands in the transcript.
	p := &scriptedProvider{script: []llm.Message{
		{Role: llm.RoleAssistant, StopReason: llm.StopError, Error: "transient"},
		{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("recovered")}},
	}}
	a := newTestAgent(p, &echoTool{})
	tr, _ := a.NewSession("t")
	_, runErr := collect(t, a, tr, "go")
	if runErr != nil {
		t.Fatal(runErr)
	}
	msgs := tr.Messages()
	if len(msgs) != 3 { // system, user, assistant(recovered)
		t.Fatalf("transcript len = %d: %+v", len(msgs), msgs)
	}
	last := msgs[len(msgs)-1]
	if last.StopReason == llm.StopError || last.Content[0].Text != "recovered" {
		t.Fatalf("tail = %+v", last)
	}
}

func TestLoopBeforeHookBlocks(t *testing.T) {
	p := &scriptedProvider{script: []llm.Message{
		{
			Role: llm.RoleAssistant, StopReason: llm.StopToolUse,
			Content: []llm.Block{{Kind: llm.BlockToolCall, ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"text":"secret"}`)}},
		},
		{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("ok")}},
	}}
	echo := &echoTool{}
	a := newTestAgent(p, echo)
	a.cfg.Before = func(call llm.Block) (bool, string) {
		var args struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(call.Arguments, &args)
		if strings.Contains(args.Text, "secret") {
			return true, "secrets not allowed"
		}
		return false, ""
	}
	tr, _ := a.NewSession("t")
	_, runErr := collect(t, a, tr, "go")
	if runErr != nil {
		t.Fatal(runErr)
	}
	if len(echo.calls) != 0 {
		t.Fatalf("blocked tool still executed: %v", echo.calls)
	}
	// Blocked call produced an error tool result for the model.
	second := p.streams[1].Messages()
	found := false
	for _, m := range second {
		for _, b := range m.Content {
			if b.Kind == llm.BlockToolResult && b.IsError && strings.Contains(b.Content[0].Text, "blocked") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("no blocked error result in follow-up context")
	}
}

func TestLoopAfterHookRewrites(t *testing.T) {
	p := &scriptedProvider{script: []llm.Message{
		{
			Role: llm.RoleAssistant, StopReason: llm.StopToolUse,
			Content: []llm.Block{{Kind: llm.BlockToolCall, ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"text":"x"}`)}},
		},
		{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("ok")}},
	}}
	a := newTestAgent(p, &echoTool{})
	a.cfg.After = func(call llm.Block, res ToolResult) ToolResult {
		return TextResult("[redacted]")
	}
	tr, _ := a.NewSession("t")
	_, runErr := collect(t, a, tr, "go")
	if runErr != nil {
		t.Fatal(runErr)
	}
	second := p.streams[1].Messages()
	for _, m := range second {
		for _, b := range m.Content {
			if b.Kind == llm.BlockToolResult && b.Content[0].Text != "[redacted]" {
				t.Fatalf("after hook did not rewrite: %q", b.Content[0].Text)
			}
		}
	}
}

func TestLoopParallelExecution(t *testing.T) {
	p := &scriptedProvider{script: []llm.Message{
		{
			Role: llm.RoleAssistant, StopReason: llm.StopToolUse,
			Content: []llm.Block{
				{Kind: llm.BlockToolCall, ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"text":"a"}`)},
				{Kind: llm.BlockToolCall, ID: "c2", Name: "echo", Arguments: json.RawMessage(`{"text":"b"}`)},
			},
		},
		{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("ok")}},
	}}
	echo := &echoTool{delay: 150 * time.Millisecond}
	a := newTestAgent(p, echo)
	a.cfg.Parallel = true
	tr, _ := a.NewSession("t")
	start := time.Now()
	_, runErr := collect(t, a, tr, "go")
	if runErr != nil {
		t.Fatal(runErr)
	}
	if time.Since(start) >= 290*time.Millisecond {
		t.Fatalf("parallel execution took %v — looks sequential", time.Since(start))
	}
	// Results appended in call order regardless of completion order.
	tail := tr.Messages()[len(tr.Messages())-2] // tool message
	if tail.Content[0].ID != "c1" || tail.Content[1].ID != "c2" {
		t.Fatalf("result order = %s, %s", tail.Content[0].ID, tail.Content[1].ID)
	}
}

func TestLoopUnknownToolSelfHeals(t *testing.T) {
	p := &scriptedProvider{script: []llm.Message{
		{
			Role: llm.RoleAssistant, StopReason: llm.StopToolUse,
			Content: []llm.Block{{Kind: llm.BlockToolCall, ID: "c1", Name: "nonexistent", Arguments: json.RawMessage(`{}`)}},
		},
		{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("recovered")}},
	}}
	a := newTestAgent(p, &echoTool{})
	tr, _ := a.NewSession("t")
	_, runErr := collect(t, a, tr, "go")
	if runErr != nil {
		t.Fatal(runErr)
	}
	second := p.streams[1].Messages()
	found := false
	for _, m := range second {
		for _, b := range m.Content {
			if b.Kind == llm.BlockToolResult && b.IsError && strings.Contains(b.Content[0].Text, "unknown tool") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("unknown tool did not produce a self-healing error result")
	}
}

func TestLoopTurnLimit(t *testing.T) {
	script := make([]llm.Message, 100)
	for i := range script {
		script[i] = llm.Message{
			Role: llm.RoleAssistant, StopReason: llm.StopToolUse,
			Content: []llm.Block{{Kind: llm.BlockToolCall, ID: fmt.Sprintf("c%d", i), Name: "echo", Arguments: json.RawMessage(`{"text":"x"}`)}},
		}
	}
	p := &scriptedProvider{script: script}
	a := newTestAgent(p, &echoTool{})
	a.cfg.MaxTurns = 3
	tr, _ := a.NewSession("t")
	_, runErr := collect(t, a, tr, "go")
	if runErr == nil || !strings.Contains(runErr.Error(), "turn limit") {
		t.Fatalf("runErr = %v", runErr)
	}
}

func TestLoopCancel(t *testing.T) {
	block := make(chan struct{})
	p := &blockingProvider{release: block}
	a := newTestAgent(p, &echoTool{})
	tr, _ := a.NewSession("t")
	out := make(chan Event, 64)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Prompt(ctx, tr, "go", out) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel must surface an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Prompt did not return after cancel")
	}
	close(block)
	for range out { // drain
	}
}

type blockingProvider struct {
	release chan struct{}
	once    sync.Once
}

func (p *blockingProvider) Name() string { return "blocking" }
func (p *blockingProvider) Caps() llm.Capabilities {
	return llm.Capabilities{}
}

func (p *blockingProvider) Stream(ctx context.Context, _ llm.Model, _ *llm.Transcript, _ llm.StreamOptions) (<-chan llm.Event, error) {
	out := make(chan llm.Event, 4)
	go func() {
		defer close(out)
		m := llm.Message{Role: llm.RoleAssistant}
		out <- llm.Event{Type: llm.EventStart, Message: &m}
		select {
		case <-p.release:
			f := llm.Message{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("late")}}
			out <- llm.Event{Type: llm.EventDone, Message: &f, Reason: llm.StopEndTurn}
		case <-ctx.Done():
			e := llm.Message{Role: llm.RoleAssistant, StopReason: llm.StopAborted, Error: "aborted"}
			out <- llm.Event{Type: llm.EventError, Message: &e, Reason: llm.StopAborted, Err: ctx.Err()}
		}
	}()
	return out, nil
}

func TestLoopContinueFromToolResult(t *testing.T) {
	// Transcript already carries user + assistant(tool_use); Continue must
	// append the tool result is NOT its job — it streams from the tail.
	p := &scriptedProvider{script: []llm.Message{
		{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("ok")}},
	}}
	a := newTestAgent(p, &echoTool{})
	tr, _ := a.NewSession("t")
	if err := tr.AppendNow(llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("hi")}}); err != nil {
		t.Fatal(err)
	}
	if err := a.Continue(context.Background(), tr, make(chan Event, 64)); err != nil {
		t.Fatal(err)
	}
	// Continue from assistant tail must be rejected.
	bad := &scriptedProvider{}
	a2 := newTestAgent(bad, &echoTool{})
	tr2, _ := a2.NewSession("t")
	_ = tr2.AppendNow(llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("hi")}})
	_ = tr2.Append(llm.Message{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("x")}})
	if err := a2.Continue(context.Background(), tr2, make(chan Event, 64)); err == nil {
		t.Fatal("continue from assistant tail must fail")
	}
}

func TestRegistryDeterministicOrder(t *testing.T) {
	r := NewRegistry(&echoTool{}, &echoTool{})
	r.Add(&echoTool{delay: time.Millisecond})
	if got := r.Decls(); len(got) != 1 || got[0].Name != "echo" {
		t.Fatalf("decls = %+v", got)
	}
}

func TestValidateArguments(t *testing.T) {
	if err := validateArguments(nil); err != nil {
		t.Fatal(err)
	}
	if err := validateArguments(json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := validateArguments(json.RawMessage(`[1,2]`)); err == nil {
		t.Fatal("array arguments must be rejected")
	}
	if err := validateArguments(json.RawMessage(`not json`)); err == nil {
		t.Fatal("invalid JSON must be rejected")
	}
}
