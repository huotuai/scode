package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"scode/internal/llm"
)

// EventType enumerates agent-loop events. Assistant streaming events pass
// through wrapped (LLM field); completed messages arrive as Assistant /
// ToolResult so consumers can persist without reassembling.
type EventType string

const (
	EvAgentStart EventType = "agent_start" // a Prompt run began
	EvTurnStart  EventType = "turn_start"  // one LLM round begins
	EvLLM        EventType = "llm"         // wrapped llm.Event (stream passthrough)
	EvAssistant  EventType = "assistant"   // completed assistant message appended
	EvToolStart  EventType = "tool_start"  // tool execution began
	EvToolEnd    EventType = "tool_end"    // tool execution finished
	EvTurnEnd    EventType = "turn_end"    // LLM round finished
	EvAgentEnd   EventType = "agent_end"   // Prompt run completed normally
	EvAgentError EventType = "agent_error" // run failed or aborted
)

type Event struct {
	Type    EventType
	LLM     *llm.Event   // set for EvLLM
	Message *llm.Message // set for EvAssistant / EvAgentError
	Call    *llm.Block   // set for EvToolStart / EvToolEnd
	Result  *ToolResult  // set for EvToolEnd
	Err     error        // set for EvAgentError
}

// Config tunes one agent.
type Config struct {
	Provider llm.Provider
	Model    llm.Model
	Stream   llm.StreamOptions
	Tools    *Registry
	Before   BeforeToolCall
	After    AfterToolCall
	Parallel bool // execute one turn's tool calls concurrently
	MaxTurns int  // safety bound; 0 = defaultMaxTurns
	Env      map[string]string
	CWD      string
}

const defaultMaxTurns = 50

// Agent runs streaming turns against a provider until the model stops
// calling tools. One Agent, one Prompt at a time.
type Agent struct {
	cfg Config
}

func New(cfg Config) *Agent {
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = defaultMaxTurns
	}
	if cfg.Tools == nil {
		cfg.Tools = NewRegistry()
	}
	return &Agent{cfg: cfg}
}

// NewSession builds a transcript with the system prompt and the registry's
// tool declarations in the leading system message — the cache-stable
// starting point for a conversation.
func (a *Agent) NewSession(systemPrompt string) (*llm.Transcript, error) {
	return llm.NormalizeContext(llm.Context{
		SystemPrompt: systemPrompt,
		Tools:        a.cfg.Tools.Decls(),
	})
}

// Prompt appends a user message and runs the agent loop to completion.
// Events stream to out, which Prompt closes on return — range over it.
// The returned error is non-nil only when the run could not complete
// (provider error, abort, turn limit).
func (a *Agent) Prompt(ctx context.Context, t *llm.Transcript, userText string, out chan<- Event) error {
	if err := t.AppendNow(llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock(userText)}}); err != nil {
		close(out)
		return err
	}
	return a.Continue(ctx, t, out)
}

// Continue resumes the loop from the current transcript tail (also the
// entry after Prompt appended the user message). The tail must not be an
// assistant message — the provider would reject the shape. out is closed
// on return.
func (a *Agent) Continue(ctx context.Context, t *llm.Transcript, out chan<- Event) error {
	defer close(out)
	msgs := t.Messages()
	if len(msgs) > 0 && msgs[len(msgs)-1].Role == llm.RoleAssistant {
		return fmt.Errorf("cannot continue: transcript tail is an assistant message")
	}
	r := &run{a: a, ctx: ctx, out: out}
	return r.loop(t)
}

// run carries one loop execution's wiring.
type run struct {
	a   *Agent
	ctx context.Context
	out chan<- Event
}

func (r *run) emit(ev Event) bool {
	select {
	case r.out <- ev:
		return true
	case <-r.ctx.Done():
		return false
	}
}

// fail records the failure as an error assistant message in the transcript
// (the run is part of the conversation) and reports it.
func (r *run) fail(t *llm.Transcript, err error) error {
	stop := llm.StopError
	if r.ctx.Err() != nil {
		stop = llm.StopAborted
	}
	msg := llm.Message{
		Role:       llm.RoleAssistant,
		TS:         time.Now().UnixMilli(),
		StopReason: stop,
		Error:      err.Error(),
	}
	_ = t.Append(msg) // best effort: transcript may itself be the problem
	m := msg
	r.emit(Event{Type: EvAgentError, Message: &m, Err: err})
	return err
}

func (r *run) loop(t *llm.Transcript) error {
	if !r.emit(Event{Type: EvAgentStart}) {
		return r.ctx.Err()
	}
	for turn := 0; turn < r.a.cfg.MaxTurns; turn++ {
		if err := r.ctx.Err(); err != nil {
			return r.fail(t, err)
		}
		if !r.emit(Event{Type: EvTurnStart}) {
			return r.fail(t, r.ctx.Err())
		}
		stop, err := r.turn(t)
		if err != nil {
			return r.fail(t, err)
		}
		if !r.emit(Event{Type: EvTurnEnd}) {
			return r.fail(t, r.ctx.Err())
		}
		if stop {
			r.emit(Event{Type: EvAgentEnd})
			return nil
		}
	}
	return r.fail(t, fmt.Errorf("turn limit exceeded (%d)", r.a.cfg.MaxTurns))
}

// turnRetries re-attempts an LLM round whose stream failed with a
// provider-side error (the transcript tail is unchanged, so the retry is
// a pure replay — pi's retryAssistantCall). Aborts never retry.
const turnRetries = 2

// turn streams one assistant message; when it requests tools, executes
// them and appends the results. Returns stop=true when the loop should
// end (assistant turn without tool calls). A provider error is retried
// up to turnRetries times with backoff; only the winning attempt is
// appended to the transcript.
func (r *run) turn(t *llm.Transcript) (bool, error) {
	var final *llm.Message
	for attempt := 0; ; attempt++ {
		events, err := r.a.cfg.Provider.Stream(r.ctx, r.a.cfg.Model, t, r.a.cfg.Stream)
		if err != nil {
			return false, err
		}
		for ev := range events {
			e := ev
			if !r.emit(Event{Type: EvLLM, LLM: &e}) {
				return false, r.ctx.Err()
			}
			if ev.Terminal() {
				final = ev.Message
			}
		}
		if final == nil {
			return false, fmt.Errorf("provider stream ended without a terminal event")
		}
		if final.StopReason == llm.StopError && attempt < turnRetries {
			select {
			case <-r.ctx.Done():
				return false, r.ctx.Err()
			case <-time.After(time.Second << uint(attempt)):
			}
			continue
		}
		break
	}
	msg := *final
	msg.TS = time.Now().UnixMilli()
	if err := t.Append(msg); err != nil {
		return false, err
	}
	if !r.emit(Event{Type: EvAssistant, Message: &msg}) {
		return false, r.ctx.Err()
	}

	switch msg.StopReason {
	case llm.StopError:
		return false, fmt.Errorf("%s", msg.Error)
	case llm.StopAborted:
		return false, r.ctx.Err()
	case llm.StopToolUse:
		return false, r.tools(t, msg)
	default: // stop, length
		return true, nil
	}
}

// tools runs every tool call in one assistant message and appends a
// single tool-result message. Sequential by default; Parallel fans the
// executions out but results are appended in call order so the
// transcript stays deterministic.
func (r *run) tools(t *llm.Transcript, asst llm.Message) error {
	var calls []llm.Block
	for _, b := range asst.Content {
		if b.Kind == llm.BlockToolCall {
			calls = append(calls, b)
		}
	}
	results := make([]llm.Block, len(calls))
	run := func(i int) {
		res := r.one(calls[i])
		results[i] = llm.Block{
			Kind:    llm.BlockToolResult,
			ID:      calls[i].ID,
			Content: res.Content,
			IsError: res.IsError,
		}
	}
	if r.a.cfg.Parallel && len(calls) > 1 {
		var wg sync.WaitGroup
		for i := range calls {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				run(i)
			}(i)
		}
		wg.Wait()
	} else {
		for i := range calls {
			run(i)
		}
	}
	return t.AppendNow(llm.Message{Role: llm.RoleTool, Content: results})
}

// one executes a single call through the hook pipeline: Before (veto),
// argument validation, execution, After (rewrite). Every failure path
// yields an ErrorResult the model can react to — never a dropped call.
func (r *run) one(call llm.Block) ToolResult {
	c := call
	if !r.emit(Event{Type: EvToolStart, Call: &c}) {
		return ErrorResult("aborted before execution")
	}

	var res ToolResult
	tool, ok := r.a.cfg.Tools.Get(call.Name)
	blocked, reason := false, ""
	if r.a.cfg.Before != nil {
		blocked, reason = r.a.cfg.Before(call)
	}
	argsErr := validateArguments(call.Arguments)
	switch {
	case !ok:
		res = ErrorResult(fmt.Sprintf("unknown tool %q", call.Name))
	case blocked:
		res = ErrorResult(fmt.Sprintf("tool call blocked: %s", reason))
	case argsErr != nil:
		res = ErrorResult(fmt.Sprintf("invalid arguments for %q: arguments must be a JSON object", call.Name))
	default:
		tc := ToolContext{Ctx: r.ctx, Env: r.a.cfg.Env, CWD: r.a.cfg.CWD}
		res = tool.Execute(tc, call.Arguments)
	}
	if r.a.cfg.After != nil {
		res = r.a.cfg.After(call, res)
	}
	out := res
	r.emit(Event{Type: EvToolEnd, Call: &c, Result: &out})
	return res
}
