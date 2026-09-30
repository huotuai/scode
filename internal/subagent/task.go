package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"scode/internal/agent"
	"scode/internal/i18n"
	"scode/internal/llm"
	"scode/internal/tools"
)

// ToolName is the permanently registered delegation tool.
const ToolName = "task"

// ReportFooterPrefix opens the delegate report's trailing stats line.
// The footer rides the TOOL RESULT (model-facing), so it stays English
// and stable — the TUI's preview filter (tui.taskReportPreview) keys on
// this exact prefix to skip the line.
const ReportFooterPrefix = "(sub-agent "

// noReportText is the stand-in when the delegate ended without text
// (model-facing, see ReportFooterPrefix).
const noReportText = "(the sub-agent produced no text report)"

// readOnlyTools is the delegate's fixed toolset (single source: the
// registry build below and the /agents badge count).
var readOnlyTools = []agent.Tool{
	tools.ReadTool{},
	tools.GrepTool{},
	tools.FindTool{},
	tools.LsTool{},
}

// ToolCount is the delegate toolset size (the /agents list badge).
func ToolCount() int { return len(readOnlyTools) }

// Host is the host seam: everything the engine needs from cli, with
// nothing shared mutable — parallel task calls each resolve their own
// provider and build their own stack.
type Host struct {
	// Specs returns the current definitions.
	Specs func() ([]Spec, error)
	// Resolve builds a provider+model for a spec's Model string (""
	// inherits the session model); the third return is the display id.
	Resolve func(model string) (llm.Provider, llm.Model, string, error)
	// Env is the shell-env base for the nested run.
	Env func() map[string]string
	// CWD is the session working directory.
	CWD func() string
	// Sandbox resolves the per-call policy (the same fence as the
	// parent — read-only tools never trip it, but the policy rides
	// along for audit parity).
	Sandbox func() *agent.SandboxPolicy
	// Spend folds the nested run's usage into the session accounting.
	Spend func(llm.Usage)
}

// TaskTool delegates a self-contained task to an isolated sub-agent.
type TaskTool struct {
	host Host
}

// NewTaskTool wires the tool to the host seams.
func NewTaskTool(h Host) *TaskTool {
	return &TaskTool{host: h}
}

const taskDescription = `Delegate a self-contained task to an isolated sub-agent with its own context window, model, and reasoning effort. The sub-agent NEVER sees this conversation — the prompt must carry everything it needs (goal, constraints, where to look). It runs with read-only tools (read, grep, find, ls) and returns a report as this tool's result. Use it for bounded research: locating code, gathering evidence across many files, summarizing a subsystem. Prefer one delegate over reading dozens of files yourself. The agent name must be one of the configured sub-agents (see the /agents manager).`

func (t *TaskTool) Decl() llm.Tool {
	return llm.Tool{
		Name:        ToolName,
		Description: taskDescription,
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"agent":{"type":"string","description":"Name of the configured sub-agent to run."},` +
			`"prompt":{"type":"string","description":"The complete, self-contained task: goal, constraints, and where to look. The sub-agent sees ONLY this."}` +
			`},"required":["agent","prompt"],"additionalProperties":false}`),
	}
}

// subSystemPrompt is the delegate's system prompt: its own charter,
// never the parent session's.
func subSystemPrompt(s Spec) string {
	var b strings.Builder
	b.WriteString("You are a focused research sub-agent inside scode, a coding agent harness. " +
		"You have READ-ONLY tools: read, grep, find, ls. Investigate exactly the task you were given — " +
		"do not ask questions, do not attempt edits. Work efficiently: narrow searches before wide ones. " +
		"Finish with a concise factual report: findings first, with file paths and line references, then any caveats.")
	if s.Description != "" {
		b.WriteString("\n\nThis agent's charter: " + s.Description)
	}
	return b.String()
}

// Execute resolves the delegate, builds a fully isolated stack (fresh
// agent loop, fresh read-only registry, fresh transcript), and runs it
// to completion under the parent call's context — there is no turn
// cap; cancellation (esc / ctrl+c on the parent run) is the only stop
// besides the delegate finishing.
func (t *TaskTool) Execute(tc agent.ToolContext, args json.RawMessage) agent.ToolResult {
	var a struct {
		Agent  string `json:"agent"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return agent.ErrorResult("invalid arguments: " + err.Error())
	}
	a.Agent = strings.TrimSpace(a.Agent)
	a.Prompt = strings.TrimSpace(a.Prompt)
	if a.Agent == "" || a.Prompt == "" {
		return agent.ErrorResult(ToolName + " requires non-empty agent and prompt")
	}

	specs, err := t.host.Specs()
	if err != nil {
		return agent.ErrorResult("sub-agents unavailable: " + err.Error())
	}
	// The effective surface: shipped built-ins overlaid by agents.json.
	specs = All(specs)
	var spec *Spec
	var known []string
	for i := range specs {
		known = append(known, specs[i].Name)
		if specs[i].Name == a.Agent {
			spec = &specs[i]
		}
	}
	if spec == nil {
		if len(known) == 0 {
			return agent.ErrorResult("no sub-agents are configured — define one with /agents first")
		}
		return agent.ErrorResult(fmt.Sprintf("unknown sub-agent %q (configured: %s)", a.Agent, strings.Join(known, ", ")))
	}

	provider, model, modelID, err := t.host.Resolve(spec.Model)
	if err != nil {
		return agent.ErrorResult(fmt.Sprintf("sub-agent %q model %q unavailable: %v", spec.Name, spec.Model, err))
	}

	// The isolated stack: every call builds its own — nothing is shared
	// between concurrent delegates or with the parent.
	sub := agent.New(agent.Config{
		Provider: provider,
		Model:    model,
		Stream:   llm.StreamOptions{ThinkingLevel: spec.Effort},
		Tools:   agent.NewRegistry(readOnlyTools...),
		Env:     t.host.Env(),
		CWD:     t.host.CWD(),
		Sandbox: t.host.Sandbox,
	})
	tr, err := sub.NewSession(subSystemPrompt(*spec))
	if err != nil {
		return agent.ErrorResult(fmt.Sprintf("sub-agent %q session: %v", spec.Name, err))
	}

	progress := tc.Progress
	say := func(line string) {
		if progress != nil {
			progress(fmt.Sprintf("%s · %s", spec.Name, line))
		}
	}
	say(i18n.Tf("tui.tool.progressStart", modelID))

	ctx, cancel := context.WithCancel(tc.Ctx)
	defer cancel()

	out := make(chan agent.Event, 256)
	done := make(chan error, 1)
	go func() { done <- sub.PromptBlocks(ctx, tr, []llm.Block{llm.TextBlock(a.Prompt)}, out) }()

	turns, calls := 0, 0
	var report strings.Builder
	for ev := range out {
		switch ev.Type {
		case agent.EvTurnStart:
			turns++
		case agent.EvToolStart:
			calls++
			if ev.Call != nil {
				say(i18n.Tf("tui.tool.progressStep", turns, briefTool(ev.Call.Name, ev.Call.Arguments)))
			}
		case agent.EvAssistant:
			// The final text of the LAST assistant message is the report.
			if ev.Message != nil {
				var txt string
				for _, b := range ev.Message.Content {
					if b.Kind == llm.BlockText {
						txt += b.Text
					}
				}
				if strings.TrimSpace(txt) != "" {
					report.Reset()
					report.WriteString(txt)
				}
			}
			if u := ev.Message.Usage; ev.Message != nil && u != nil && u.TotalTokens() > 0 && t.host.Spend != nil {
				t.host.Spend(*u)
			}
		case agent.EvAgentError:
			if ev.Err != nil && ctx.Err() == nil {
				return agent.ErrorResult(fmt.Sprintf("sub-agent %q failed: %v", spec.Name, ev.Err))
			}
		}
	}
	if err := <-done; err != nil && ctx.Err() == nil {
		return agent.ErrorResult(fmt.Sprintf("sub-agent %q failed: %v", spec.Name, err))
	}

	footer := fmt.Sprintf("%s%s · %d turns · %d tool calls · model %s)", ReportFooterPrefix, spec.Name, turns, calls, modelID)
	text := strings.TrimSpace(report.String())
	if text == "" {
		text = noReportText
	}
	return agent.TextResult(text + "\n\n" + footer)
}

// briefTool renders "name first-args" for the progress line.
func briefTool(name string, args json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(args, &m); err == nil {
		for _, k := range []string{"pattern", "path", "file_path", "command", "query", "dir"} {
			if v, ok := m[k]; ok {
				if s, ok := v.(string); ok && s != "" {
					return name + " " + s
				}
			}
		}
	}
	return name
}
