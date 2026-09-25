// Package agent implements scode's agent loop: streaming an assistant
// turn, executing requested tool calls, appending results, and repeating
// until the model ends its turn — with before/after hooks as the
// interception point for permissions, auditing, and rewriting (pi's
// AgentTool contract, adapted to Go).
package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"scode/internal/llm"
)

// ToolContext carries execution-scoped state into a tool. Env mirrors the
// session facts pi exposes to shell commands (SCODE_MODEL & co) so tools
// can forward them without touching the system prompt (cache rule 4).
type ToolContext struct {
	Ctx context.Context
	Env map[string]string
	CWD string
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
// execution and produces an error tool result carrying reason.
type BeforeToolCall func(call llm.Block) (blocked bool, reason string)

// AfterToolCall can rewrite a result after execution (redact, truncate,
// annotate). It must treat the result as immutable and return a new one.
type AfterToolCall func(call llm.Block, res ToolResult) ToolResult

// Registry resolves tool names to implementations for one session.
type Registry struct {
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
	if _, ok := r.tools[d.Name]; !ok {
		r.order = append(r.order, d.Name)
	}
	r.tools[d.Name] = t
}

func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Decls returns the declarations in registration order (deterministic —
// the bytes go into the cache-relevant tools array).
func (r *Registry) Decls() []llm.Tool {
	out := make([]llm.Tool, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.tools[n].Decl())
	}
	return out
}

// validateArguments performs light structural validation: the arguments
// must be a JSON object. Deep JSON Schema checking arrives with the tool
// implementations (M3); execution errors remain the self-healing path —
// a tool that rejects bad arguments returns an ErrorResult the model can
// act on.
func validateArguments(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil // tools may take no arguments
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("arguments must be a JSON object: %w", err)
	}
	return nil
}
