// Package agent implements scode's agent loop: streaming an assistant
// turn, executing requested tool calls, appending results, and repeating
// until the model ends its turn — with before/after hooks as the
// interception point for permissions, auditing, and rewriting (pi's
// AgentTool contract, adapted to Go).
package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"scode/internal/llm"
)

// ToolContext carries execution-scoped state into a tool. Env mirrors the
// session facts pi exposes to shell commands (SCODE_MODEL & co) so tools
// can forward them without touching the system prompt (cache rule 4).
type ToolContext struct {
	Ctx context.Context
	Env map[string]string
	CWD string
	// Sandbox is the per-call resolved file-effect policy (dsh's
	// SandboxExecutionPolicy). nil means unfenced (danger-full-access).
	Sandbox *SandboxPolicy
	// Escalate asks the human to widen this call's sandbox mode (dsh's
	// approveEscalation channel). nil = no approval channel (fail closed).
	Escalate func(requestedMode, justification, detail string) EscalationResult
	// Progress reports a live one-line status for long-running tools
	// (the subagent engine streams "which step, which tool" while it
	// works). nil = no listener; calls must be cheap and bounded.
	Progress func(line string)
}

// EscalationResult is the closed outcome of one sandbox escalation ask.
type EscalationResult struct {
	Approved       bool
	ApplyToSession bool   // widen the session mode too ('allow_session')
	Reason         string // deny reason surfaced to the model
}

// EscalationRequest is one sandbox widening ask, audit-self-contained.
type EscalationRequest struct {
	Ctx           context.Context // the run's ctx: cancellation interrupts the ask
	Tool          string
	CallID        string
	Detail        string // the command or path being widened
	CurrentMode   string
	RequestedMode string
	Justification string
}

// SandboxPolicy is the per-call sandbox view handed to tools: the file
// effect mode plus the workspace boundary. Defined here (not in
// internal/sandbox) to keep agent the bottom of the dependency graph.
type SandboxPolicy struct {
	Mode          string // read-only | workspace-write | danger-full-access
	WorkspaceRoot string
}

// Constraint: an ABSENT policy is never permission. Tools fail closed when
// the context carries no SandboxPolicy (see internal/tools.ResolvePolicy),
// because a missing wiring would otherwise silently grant full access.
//
// UnfencedPolicy is the explicit opt-out for embedders and tests that
// intentionally run without confinement: pass it as Config.Sandbox so the
// decision is written down and greppable instead of implied.
func UnfencedPolicy(workspaceRoot string) *SandboxPolicy {
	return &SandboxPolicy{Mode: "danger-full-access", WorkspaceRoot: workspaceRoot}
}

// ToolResult is the outcome of one tool execution. Content carries the
// blocks shown to the model (text and image); IsError marks failures the
// model should react to.
type ToolResult struct {
	Content []llm.Block
	IsError bool
}

// TextResult builds a plain-text result.
func TextResult(text string) ToolResult {
	return ToolResult{Content: []llm.Block{llm.TextBlock(text)}}
}

// ErrorResult builds an error result whose message is meant to let the
// model self-correct (retry with fixed arguments, give up gracefully).
func ErrorResult(text string) ToolResult {
	return ToolResult{Content: []llm.Block{llm.TextBlock(text)}, IsError: true}
}

// Tool is one tool the agent can execute. Decl must be stable for the
// session lifetime: its bytes land in the transcript's leading system
// message, and any change would break the provider prefix cache. Tools
// that need to evolve their interface should register a new name.
type Tool interface {
	Decl() llm.Tool
	Execute(tc ToolContext, args json.RawMessage) ToolResult
}

// BeforeToolCall can veto a tool call: returning blocked=true skips
// execution and produces an error tool result carrying reason. The ctx
// is the run's: a blocking Before (approval prompt) must unblock on
// cancellation so an aborted run never strands a tool goroutine.
type BeforeToolCall func(ctx context.Context, call llm.Block) (blocked bool, reason string)

// AfterToolCall can rewrite a result after execution (redact, truncate,
// annotate). It must treat the result as immutable and return a new one.
type AfterToolCall func(call llm.Block, res ToolResult) ToolResult

// Registry resolves tool names to implementations for one session.
// It is safe for concurrent use: MCP generations swap tools from
// supervisor goroutines while the agent loop reads (Get/Decls) from
// its own and parallel tool goroutines.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	order []string
}

func NewRegistry(tools ...Tool) *Registry {
	r := &Registry{tools: map[string]Tool{}}
	for _, t := range tools {
		r.Add(t)
	}
	return r
}

func (r *Registry) Add(t Tool) {
	d := t.Decl()
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[d.Name]; !ok {
		r.order = append(r.order, d.Name)
	}
	r.tools[d.Name] = t
}

func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if t, ok := r.tools[name]; ok {
		return t, true
	}
	// Case-insensitive fallback on a MISS only: models sometimes emit a
	// tool name with a training-prior casing ("Task" for "task",
	// carried over from Claude Code transcripts) on their first call.
	// Exact names always win; the scan only runs when the exact lookup
	// already failed.
	for n, t := range r.tools {
		if strings.EqualFold(n, name) {
			return t, true
		}
	}
	return nil, false
}

// Remove unregisters a tool by name (MCP servers drop tools when their
// generation swaps or the connection dies).
func (r *Registry) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[name]; !ok {
		return
	}
	delete(r.tools, name)
	for i, n := range r.order {
		if n == name {
			r.order = append(r.order[:i:i], r.order[i+1:]...)
			break
		}
	}
}

// Decls returns the declarations in registration order (deterministic —
// the bytes go into the cache-relevant tools array).
func (r *Registry) Decls() []llm.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]llm.Tool, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.tools[n].Decl())
	}
	return out
}

// ToolContribution is one tool's system-prompt contribution (pi's
// snippet + guidelines from the tool definitions).
type ToolContribution struct {
	Name       string
	Snippet    string
	Guidelines []string
}

// PromptContributor is a tool that contributes a snippet and guidelines
// to the system prompt (pi's tool prompt contributions).
type PromptContributor interface {
	PromptContribution() (snippet string, guidelines []string)
}

// Contributions collects prompt contributions in registration order.
func (r *Registry) Contributions() []ToolContribution {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []ToolContribution
	for _, n := range r.order {
		if pc, ok := r.tools[n].(PromptContributor); ok {
			snippet, guidelines := pc.PromptContribution()
			out = append(out, ToolContribution{Name: n, Snippet: snippet, Guidelines: guidelines})
		}
	}
	return out
}
