package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"scode/internal/llm"
)

// EventType enumerates agent-loop events. Assistant streaming events pass
// through wrapped (LLM field); completed messages arrive as Assistant /
// ToolResult so consumers can persist without reassembling.
type EventType string

const (
	EvAgentStart   EventType = "agent_start"   // a Prompt run began
	EvTurnStart    EventType = "turn_start"    // one LLM round begins
	EvLLM          EventType = "llm"           // wrapped llm.Event (stream passthrough)
	EvAssistant    EventType = "assistant"     // completed assistant message appended
	EvUser         EventType = "user"          // user message appended (prompt or steered)
	EvToolStart    EventType = "tool_start"    // tool execution began
	EvToolProgress EventType = "tool_progress" // live status from a long-running tool
	EvToolEnd      EventType = "tool_end"      // tool execution finished
	EvTurnEnd      EventType = "turn_end"      // LLM round finished
	EvAgentEnd     EventType = "agent_end"     // Prompt run completed normally
	EvAgentError   EventType = "agent_error"   // run failed or aborted
)

type Event struct {
	Type     EventType
	LLM      *llm.Event   // set for EvLLM
	Message  *llm.Message // set for EvAssistant / EvUser / EvAgentError
	Call     *llm.Block   // set for EvToolStart / EvToolProgress / EvToolEnd
	Result   *ToolResult  // set for EvToolEnd
	Progress string       // set for EvToolProgress
	Err      error        // set for EvAgentError
}

// Config tunes one agent.
type Config struct {
	Provider llm.Provider
	Model    llm.Model
	Stream   llm.StreamOptions
	Tools    *Registry
	Before   BeforeToolCall
	After    AfterToolCall
	// Sandbox resolves the per-call file-effect policy (mode switches
	// take effect on the NEXT restricted call). nil = unfenced.
	Sandbox func() *SandboxPolicy
	// SandboxEscalation resolves a per-call sandbox widening through the
	// session's approval channel (nil = fail closed).
	SandboxEscalation func(EscalationRequest) EscalationResult
	// ToolExecution is pi's toolExecution: "parallel" (default) or
	// "sequential". A tool reporting ExecutionMode() == "sequential"
	// forces its batch sequential.
	ToolExecution string
	// Retry is pi's settings.retry policy for the assistant call.
	Retry RetryPolicy
	// SteeringMode and FollowUpMode are pi's queue drain modes
	// (default one-at-a-time).
	SteeringMode QueueMode
	FollowUpMode QueueMode
	Env          map[string]string
	CWD          string
}

// QueueMode is pi's queue drain mode for steering/follow-up messages.
type QueueMode string

const (
	QueueOneAtATime QueueMode = "one-at-a-time" // default
	QueueAll        QueueMode = "all"
)

// Agent runs streaming turns against a provider until the model stops
// calling tools. One Agent, one Prompt at a time. Like pi's runLoop
// there is no turn cap: the loop ends when the model ends its turn or
// the caller aborts.
type Agent struct {
	cfg Config

	queueMu  sync.Mutex
	steer    []string
	followUp []string
}

func New(cfg Config) *Agent {
	if cfg.Tools == nil {
		cfg.Tools = NewRegistry()
	}
	if cfg.Retry == (RetryPolicy{}) {
		cfg.Retry = DefaultRetryPolicy()
	}
	if cfg.SteeringMode == "" {
		cfg.SteeringMode = QueueOneAtATime
	}
	if cfg.FollowUpMode == "" {
		cfg.FollowUpMode = QueueOneAtATime
	}
	return &Agent{cfg: cfg}
}

// SetModel swaps the provider/model (and tool env) for subsequent
// turns. Callers must guarantee no run is in flight: the loop reads
// cfg without a lock.
func (a *Agent) SetModel(p llm.Provider, m llm.Model, env map[string]string) {
	a.cfg.Provider = p
	a.cfg.Model = m
	if env != nil {
		a.cfg.Env = env
	}
}

// SetThinkingLevel swaps the reasoning effort for subsequent runs
// ("" = provider default, "off"|"low"|"medium"|"high"). Same
// no-run-in-flight contract as SetModel.
func (a *Agent) SetThinkingLevel(level string) {
	a.cfg.Stream.ThinkingLevel = level
}

// SetSession swaps the session identity for subsequent runs: the
// prompt-cache routing key and the tool env both name the session
// (cli /new rebinds the app to a fresh session file in place). Same
// no-run-in-flight contract as SetModel.
func (a *Agent) SetSession(id string, env map[string]string) {
	a.cfg.Stream.PromptCacheKey = id
	if env != nil {
		a.cfg.Env = env
	}
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
// (provider error or abort).
func (a *Agent) Prompt(ctx context.Context, t *llm.Transcript, userText string, out chan<- Event) error {
	return a.PromptBlocks(ctx, t, []llm.Block{llm.TextBlock(userText)}, out)
}

// PromptBlocks is Prompt with rich content: text plus image blocks
// (pi's user message with attachments). An empty content slice is a
// no-op prompt (the loop continues from the transcript tail).
func (a *Agent) PromptBlocks(ctx context.Context, t *llm.Transcript, content []llm.Block, out chan<- Event) error {
	if len(content) == 0 {
		return a.Continue(ctx, t, out)
	}
	if err := t.AppendNow(llm.Message{Role: llm.RoleUser, Content: content}); err != nil {
		close(out)
		return err
	}
	return a.Continue(ctx, t, out)
}

// Steer queues a steering message for injection at the next safe
// point — between LLM turns or when the loop is about to stop (pi's
// steering). Safe to call from any goroutine while a run is active.
func (a *Agent) Steer(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	a.steer = append(a.steer, text)
}

// FollowUp queues a message delivered only when the loop would stop,
// keeping the run alive (pi's follow-up queue).
func (a *Agent) FollowUp(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	a.followUp = append(a.followUp, text)
}

// drain dequeues steering/follow-up messages per the queue mode (pi's
// PendingMessageQueue): one-at-a-time takes the oldest, all takes
// everything.
func (a *Agent) drain(queue *[]string, mode QueueMode) []string {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	if len(*queue) == 0 {
		return nil
	}
	if mode == QueueAll {
		out := *queue
		*queue = nil
		return out
	}
	out := (*queue)[:1]
	*queue = (*queue)[1:]
	return out
}

// Continue resumes the loop from the current transcript tail (also the
// entry after Prompt appended the user message). The tail must not be a
// COMPLETED assistant message — re-asking without new input is a bug.
// A tail carrying a recorded failure (error/aborted) is fine: request
// builders replay it as text, so Continue re-issues the failed LLM
// call — the overflow-recovery path relies on this. out is closed on
// return.
func (a *Agent) Continue(ctx context.Context, t *llm.Transcript, out chan<- Event) error {
	defer close(out)
	msgs := t.Messages()
	if len(msgs) > 0 && msgs[len(msgs)-1].Role == llm.RoleAssistant {
		tail := msgs[len(msgs)-1]
		if tail.StopReason != llm.StopError && tail.StopReason != llm.StopAborted {
			return fmt.Errorf("cannot continue: transcript tail is an assistant message")
		}
	}
	r := &run{a: a, ctx: ctx, out: out, model: a.cfg.Model, thinking: a.cfg.Stream.ThinkingLevel}
	return r.loop(t)
}

// run carries one loop execution's wiring.
type run struct {
	a   *Agent
	ctx context.Context
	out chan<- Event

	model    llm.Model // request-time model/thinking are run-scoped copies
	thinking string    // of the config (a run never changes them mid-flight)
}

func (r *run) emit(ev Event) bool {
	select {
	case r.out <- ev:
		return true
	case <-r.ctx.Done():
		return false
	}
}

// fail reports a run failure. The stream's own terminal error message
// is already in the transcript (finishTurn appends it, like pi keeping
// the failed assistant message in state); only failures that never
// produced a message get a synthetic record — content-less, with the
// error field set. Request transforms skip such messages on replay, so
// the failure never poisons later requests.
func (r *run) fail(t *llm.Transcript, err error) error {
	msgs := t.Messages()
	recorded := len(msgs) > 0 && msgs[len(msgs)-1].Role == llm.RoleAssistant &&
		(msgs[len(msgs)-1].StopReason == llm.StopError || msgs[len(msgs)-1].StopReason == llm.StopAborted)
	if !recorded {
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
	r.emit(Event{Type: EvAgentError, Err: err})
	return err
}

// loop is pi's runLoop: an inner loop processing tool calls and
// steered messages, wrapped by an outer loop that keeps the run alive
// for queued follow-ups. No turn cap — the model ending its turn (or
// the caller aborting) is the only exit.
func (r *run) loop(t *llm.Transcript) error {
	if !r.emit(Event{Type: EvAgentStart}) {
		return r.ctx.Err()
	}
	// Steering queued while waiting is injected first (pi).
	pending := r.a.drain(&r.a.steer, r.a.cfg.SteeringMode)
	for {
		hasMoreToolCalls := true
		for hasMoreToolCalls || len(pending) > 0 {
			if err := r.ctx.Err(); err != nil {
				return r.fail(t, err)
			}
			if !r.emit(Event{Type: EvTurnStart}) {
				return r.fail(t, r.ctx.Err())
			}
			for _, m := range textsToMessages(pending) {
				if err := r.appendInjected(t, m); err != nil {
					return r.fail(t, err)
				}
			}
			pending = nil

			stop, err := r.turn(t)
			if err != nil {
				return r.fail(t, err)
			}
			if !r.emit(Event{Type: EvTurnEnd}) {
				return r.fail(t, r.ctx.Err())
			}

			hasMoreToolCalls = !stop
			// Mid-run steering: corrections land between turns, so the
			// next LLM call sees them (pi's getSteeringMessages drain).
			pending = r.a.drain(&r.a.steer, r.a.cfg.SteeringMode)
		}
		// The loop would stop here; queued follow-ups keep it alive
		// (pi's getFollowUpMessages).
		if followUps := r.a.drain(&r.a.followUp, r.a.cfg.FollowUpMode); len(followUps) > 0 {
			pending = followUps
			continue
		}
		break
	}
	r.emit(Event{Type: EvAgentEnd})
	return nil
}

// textsToMessages wraps steered/follow-up texts as user messages.
func textsToMessages(texts []string) []llm.Message {
	var out []llm.Message
	for _, text := range texts {
		out = append(out, llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock(text)}})
	}
	return out
}

// appendInjected appends one injected message (steering or follow-up)
// and emits its lifecycle event.
func (r *run) appendInjected(t *llm.Transcript, m llm.Message) error {
	llm.NormalizeArguments(&m)
	if err := t.AppendNow(m); err != nil {
		return err
	}
	switch m.Role {
	case llm.RoleUser:
		u := m
		if !r.emit(Event{Type: EvUser, Message: &u}) {
			return r.ctx.Err()
		}
	case llm.RoleAssistant:
		u := m
		if !r.emit(Event{Type: EvAssistant, Message: &u}) {
			return r.ctx.Err()
		}
	}
	return nil
}

// turnRetries re-attempts an LLM round whose stream failed with a
// retryable provider-side error (the transcript tail is unchanged, so
// the retry is a pure replay — pi's retryAssistantCall). Aborts never
// retry.

// RetryPolicy mirrors pi's settings.retry: bounded attempts with
// exponential backoff (baseDelayMs * 2^(attempt-1)), each delay capped
// at maxAgentDelayMs.
type RetryPolicy struct {
	Enabled         bool
	MaxRetries      int // 0 = no retries; the initial call never counts
	BaseDelayMs     int
	MaxAgentDelayMs int
}

// DefaultRetryPolicy is pi's default: 3 retries, 2s base, 60s cap.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{Enabled: true, MaxRetries: 3, BaseDelayMs: 2000, MaxAgentDelayMs: 60_000}
}

// retryDelayMs is pi's retryDelayMs: baseDelayMs * 2^(attempt-1),
// capped (attempt is 1-indexed).
func (p RetryPolicy) retryDelayMs(attempt int) int {
	d := p.BaseDelayMs << (attempt - 1)
	if d < 0 || d > p.MaxAgentDelayMs { // overflow guard
		d = p.MaxAgentDelayMs
	}
	return d
}

// Error classification ported from pi's utils/retry.ts: a WHITELIST —
// only errors matching known-transient patterns retry; the small
// blacklist short-circuits quota/billing exhaustion; anything
// unrecognized fails fast.
var nonRetryableProviderLimitError = regexp.MustCompile(`(?i)(GoUsageLimitError|FreeUsageLimitError|Monthly usage limit reached|available balance|insufficient_quota|out of budget|quota exceeded|billing)`)

var retryableProviderError = regexp.MustCompile(`(?i)(overloaded|currently experiencing high demand|rate.?limit|too many requests|429|500|502|503|504|520|524|service.?unavailable|server.?error|internal.?error|provider.?returned.?error|exceeded request buffer limit while retrying upstream|network.?error|connection.?error|connection.?refused|connection.?lost|other side closed|fetch failed|getaddrinfo|ENOTFOUND|EAI_AGAIN|upstream.?connect|reset before headers|socket hang up|socket connection was closed|timed? out|timeout|terminated|websocket.?closed|websocket.?error|ended without|stream ended before message_stop|stream ended before a terminal( response)? event|http2 request did not get a response|retry delay|you can retry your request|try your request again|please retry your request|ResourceExhausted)`)

// retryableError is pi's isRetryableAssistantError.
func retryableError(msg string) bool {
	if nonRetryableProviderLimitError.MatchString(msg) {
		return false
	}
	return retryableProviderError.MatchString(msg)
}

// turn streams one assistant message; when it requests tools, executes
// them and appends the results. Returns stop=true when the loop should
// end (assistant turn without tool calls). A retryable provider error
// is retried under the retry policy with pi's backoff; only the
// winning attempt is appended to the transcript.
func (r *run) turn(t *llm.Transcript) (bool, error) {
	maxRetries := 0
	if r.a.cfg.Retry.Enabled {
		maxRetries = r.a.cfg.Retry.MaxRetries
	}
	for attempt := 0; ; attempt++ {
		events, err := r.a.cfg.Provider.Stream(r.ctx, r.model, t, r.streamOptions())
		if err != nil {
			return false, err
		}
		// final is attempt-scoped: a stale pointer from a failed attempt
		// must never leak into the next one.
		var final *llm.Message
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
		if final.StopReason == llm.StopError && attempt < maxRetries && retryableError(final.Error) {
			delay := r.a.cfg.Retry.retryDelayMs(attempt + 1) // attempt is 1-indexed (pi)
			select {
			case <-r.ctx.Done():
				return false, r.ctx.Err()
			case <-time.After(time.Duration(delay) * time.Millisecond):
			}
			continue
		}
		return r.finishTurn(t, final)
	}
}

// streamOptions resolves this turn's stream options (thinking level
// is request-time state since hooks may replace it).
func (r *run) streamOptions() llm.StreamOptions {
	opts := r.a.cfg.Stream
	opts.ThinkingLevel = r.thinking
	return opts
}

// finishTurn appends the winning assistant message and routes by stop
// reason.
func (r *run) finishTurn(t *llm.Transcript, final *llm.Message) (bool, error) {
	msg := *final
	msg.TS = time.Now().UnixMilli()
	// Provider-produced tool arguments must be marshalable before they
	// enter the transcript: invalid JSON fragments would otherwise break
	// persistence and replay.
	llm.NormalizeArguments(&msg)
	if msg.StopReason == llm.StopError || msg.StopReason == llm.StopAborted {
		// The failed turn is recorded like any other message (pi keeps
		// it in state, partial content included); request transforms
		// skip it on replay. fail() reports without re-appending.
		_ = t.Append(msg)
		if msg.StopReason == llm.StopError {
			return false, fmt.Errorf("%s", msg.Error)
		}
		if err := r.ctx.Err(); err != nil {
			return false, err
		}
		return false, fmt.Errorf("%s", msg.Error)
	}
	if err := t.Append(msg); err != nil {
		return false, err
	}
	if !r.emit(Event{Type: EvAssistant, Message: &msg}) {
		return false, r.ctx.Err()
	}

	if msg.StopReason == llm.StopToolUse {
		return false, r.tools(t, msg)
	}
	if msg.StopReason == llm.StopLength && hasToolCalls(msg) {
		// Output ran out mid-tool-call: the arguments may be truncated.
		// Answer every call with an error result so the transcript stays
		// valid and the model can re-issue them (pi's
		// failToolCallsFromTruncatedMessage, including its message text)
		// — otherwise the tail is an assistant message with unanswered
		// calls that no provider accepts.
		for _, b := range msg.Content {
			if b.Kind != llm.BlockToolCall {
				continue
			}
			result := llm.Block{
				Kind: llm.BlockToolResult,
				ID:   b.ID,
				Content: []llm.Block{llm.TextBlock(fmt.Sprintf(
					"Tool call %q was not executed: the response hit the output token limit, so its arguments may be truncated. Re-issue the tool call with complete arguments.",
					b.Name))},
				IsError: true,
			}
			if err := t.AppendNow(llm.Message{Role: llm.RoleTool, Content: []llm.Block{result}}); err != nil {
				return false, err
			}
		}
		return false, nil // continue the loop; the model recovers
	}
	return true, nil // stop, plain length
}

func hasToolCalls(m llm.Message) bool {
	for _, b := range m.Content {
		if b.Kind == llm.BlockToolCall {
			return true
		}
	}
	return false
}

// tools runs every tool call in one assistant message. Parallel by
// default (pi's executeToolCallsParallel); "sequential" config or a
// tool declaring ExecutionMode() == "sequential" forces the batch
// sequential (pi's rule). Results are appended in call order — one
// toolResult message per call (pi's shape) — so the transcript stays
// deterministic.
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
			Name:    calls[i].Name, // pi's ToolResultMessage.toolName (Google functionResponse needs it)
			Content: res.Content,
			IsError: res.IsError,
		}
	}
	sequential := r.a.cfg.ToolExecution == "sequential"
	if !sequential {
		for _, c := range calls {
			if tool, ok := r.a.cfg.Tools.Get(c.Name); ok {
				if sm, is := tool.(interface{ ExecutionMode() string }); is && sm.ExecutionMode() == "sequential" {
					sequential = true
					break
				}
			}
		}
	}
	if sequential || len(calls) <= 1 {
		for i := range calls {
			run(i)
			if r.ctx.Err() != nil {
				break
			}
		}
	} else {
		var wg sync.WaitGroup
		for i := range calls {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				run(i)
			}(i)
		}
		wg.Wait()
	}
	for _, b := range results {
		if err := t.AppendNow(llm.Message{Role: llm.RoleTool, Content: []llm.Block{b}}); err != nil {
			return err
		}
	}
	return nil
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
		blocked, reason = r.a.cfg.Before(r.ctx, call)
	}
	var argsErr error
	args := call.Arguments
	if ok {
		// pi validates (and coerces) arguments against the tool's
		// declared JSON Schema before execution; the tool runs with
		// the coerced value.
		var coerced json.RawMessage
		coerced, argsErr = llm.ValidateArguments(tool.Decl().Parameters, call.Arguments)
		if argsErr == nil {
			args = coerced
		}
	}
	switch {
	case !ok:
		res = ErrorResult(fmt.Sprintf("unknown tool %q", call.Name))
	case blocked:
		res = ErrorResult(fmt.Sprintf("tool call blocked: %s", reason))
	case argsErr != nil:
		res = ErrorResult(fmt.Sprintf("invalid arguments for %q: %v", call.Name, argsErr))
	default:
		tc := ToolContext{Ctx: r.ctx, Env: r.a.cfg.Env, CWD: r.a.cfg.CWD}
		tc.Progress = func(line string) {
			pc := call
			r.emit(Event{Type: EvToolProgress, Call: &pc, Progress: line})
		}
		if r.a.cfg.Sandbox != nil {
			tc.Sandbox = r.a.cfg.Sandbox()
		}
		if r.a.cfg.SandboxEscalation != nil {
			// Close over the call's identity: the tool supplies only the
			// mode/justification/detail.
			tc.Escalate = func(requestedMode, justification, detail string) EscalationResult {
				effective := "danger-full-access"
				if tc.Sandbox != nil && tc.Sandbox.Mode != "" {
					effective = tc.Sandbox.Mode
				}
				return r.a.cfg.SandboxEscalation(EscalationRequest{
					Ctx:           r.ctx,
					Tool:          call.Name,
					CallID:        call.ID,
					Detail:        detail,
					CurrentMode:   effective,
					RequestedMode: requestedMode,
					Justification: justification,
				})
			}
		}
		res = tool.Execute(tc, args)
	}
	if r.a.cfg.After != nil {
		res = r.a.cfg.After(call, res)
	}
	out := res
	r.emit(Event{Type: EvToolEnd, Call: &c, Result: &out})
	return res
}
