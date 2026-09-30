// Package planmode implements plan mode: a collaboration mode whose
// semantics follow deepseek-harness's plan-mode package
// (packages/plan/plan-mode), translated to scode's delta/append-only
// machinery, with one deliberate divergence — write operations are HARD
// denied (Claude Code-style coupling), where dsh relies on prompt
// guidance alone. Design: docs/design-permission-plan-desktop.md.
//
// Pieces: the Controller (state + dedup + OnChange hook the host wires
// to persistence and transcript deltas), Deny (the mode's built-in
// permission guard — a whitelist carve-out is not expressible in the
// generic rule lists), and the permanently registered exit_plan_mode
// tool (stable tool catalog across mode transitions; dsh's choice).
package planmode

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"scode/internal/agent"
	"scode/internal/llm"
	"scode/internal/permission"
)

// Section is the system-prompt section name carrying the plan guidance
// (appended/removed as a transcript delta on mode transitions).
const Section = "plan"

// Guidance is the prompt text injected while plan mode is active. It is
// the mode's full behavioral contract: what is denied, what to do
// instead, and how to exit.
const Guidance = `Plan mode is active. You are researching and planning, NOT executing:
- The write and edit tools are denied, except inside .scode/plan/ where you may save plan drafts.
- bash commands with write effects (redirection, rm/mv/cp, tee, dd, chmod, pipes into a shell, mutating git subcommands) are denied; read-only commands are fine.
- Do not make changes. Investigate with read-only tools, then call exit_plan_mode with the complete plan as markdown starting with a # heading.
- The user will approve the plan (plan mode exits; execute from your next step) or send it back with feedback — revise and present again.`

// Controller owns the plan-mode state. Transitions dedupe (repeated
// selection of the current state is a no-op) and fire OnChange once per
// actual change; the host wires OnChange to mode-entry persistence and
// the transcript section delta.
type Controller struct {
	mu       sync.Mutex
	active   bool
	OnChange func(active bool)
}

// Active reports the current state.
func (c *Controller) Active() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

// Set switches the mode, returning false when already in the target
// state (delta dedup: repeated switches leave no trace).
func (c *Controller) Set(active bool) bool {
	c.mu.Lock()
	if c.active == active {
		c.mu.Unlock()
		return false
	}
	c.active = active
	onChange := c.OnChange
	c.mu.Unlock()
	if onChange != nil {
		onChange(active)
	}
	return true
}

// Restore sets the state without firing OnChange (load-time replay:
// the state is already durable; the caller re-adds the prompt delta).
func (c *Controller) Restore(active bool) {
	c.mu.Lock()
	c.active = active
	c.mu.Unlock()
}

// PlanDirGlob is the writable whitelist inside plan mode.
const PlanDirGlob = ".scode/plan/**"

// Deny is plan mode's built-in permission guard. It returns blocked=true
// for write/edit outside .scode/plan/ and for bash commands with write
// features; everything else falls through to the configured rule lists
// (MCP tools included — their server defaultPolicy still applies).
func Deny(call llm.Block, cwd string) (blocked bool, reason string) {
	switch call.Name {
	case "write", "edit":
		var args struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(call.Arguments, &args)
		if permission.MatchPathGlob(PlanDirGlob, args.Path, cwd) {
			return false, ""
		}
		return true, fmt.Sprintf("plan mode is read-only: %s is denied outside %s; "+
			"research with read-only tools and submit the plan via exit_plan_mode", call.Name, PlanDirGlob)
	case "bash":
		var args struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(call.Arguments, &args)
		if HasWriteFeatures(args.Command) {
			return true, "plan mode is read-only: this bash command has write effects; " +
				"use read-only commands and submit the plan via exit_plan_mode"
		}
	}
	return false, ""
}

// Shell-level write heuristics (documented as best-effort: obfuscation
// defeats pattern matching; the hard guarantee awaits OS-level
// sandboxing). Covers redirection, mutating coreutils, pipes into a
// shell, and mutating git subcommands.
var (
	redirectRe  = regexp.MustCompile(`(?:^|[\s|&])(?:\d?>|\d?>>)\s*\S`)
	writeCmdsRe = regexp.MustCompile(`\b(rm|mv|cp|dd|chmod|chown|tee|truncate|shred|mkdir|rmdir|touch|ln)\b`)
	pipeShellRe = regexp.MustCompile(`\|\s*(sudo\s+)?(ba|z|fi)?sh\b`)
)

// gitMutatingSubcommands are the git verbs that change the repo or its
// history (checkout/switch/restore included: they rewrite the working
// tree or HEAD).
var gitMutatingSubcommands = map[string]bool{
	"add": true, "commit": true, "push": true, "merge": true, "rebase": true,
	"reset": true, "clean": true, "apply": true, "am": true, "stash": true,
	"tag": true, "branch": true, "rm": true, "mv": true, "checkout": true,
	"switch": true, "restore": true,
}

// gitMutation parses "git [flags] <subcommand>" with a tiny tokenizer
// (regexes cannot skip flag-argument pairs like "git -C repo push").
func gitMutation(cmd string) bool {
	fields := strings.Fields(cmd)
	for i, f := range fields {
		if f != "git" {
			continue
		}
		j := i + 1
		for j < len(fields) && strings.HasPrefix(fields[j], "-") {
			// Flags taking a separate value: -C <dir>, -c <k=v>.
			if fields[j] == "-C" || fields[j] == "-c" {
				j++
			}
			j++
		}
		if j < len(fields) && gitMutatingSubcommands[fields[j]] {
			return true
		}
	}
	return false
}

// HasWriteFeatures reports whether a shell command shows write effects.
func HasWriteFeatures(cmd string) bool {
	return redirectRe.MatchString(cmd) ||
		writeCmdsRe.MatchString(cmd) ||
		pipeShellRe.MatchString(cmd) ||
		gitMutation(cmd)
}

// ---------------------------------------------------------------------------
// exit_plan_mode (dsh's tool contract, plan-as-parameter)
// ---------------------------------------------------------------------------

// ExitToolName is the permanently registered tool name.
const ExitToolName = "exit_plan_mode"

const exitDescription = `Use only in plan mode. Present your plan for the user's review and, on approval, leave plan mode. Send the COMPLETE plan as markdown, starting with a # heading that names it. The user may approve (carry out the plan from your next step) or keep planning — their feedback comes back in the tool result; revise and present again.`

// Reviewer is the host's plan-review channel (the CLI approver
// implements it; a future serve transport reimplements over RPC).
type Reviewer interface {
	ReviewPlan(ctx agent.ToolContext, plan string) (approved bool, feedback string)
}

// ExitTool presents the completed plan for user review.
type ExitTool struct {
	ctl *Controller
	rev Reviewer
}

// NewExitTool wires the exit tool to the mode controller and reviewer.
func NewExitTool(ctl *Controller, rev Reviewer) *ExitTool {
	return &ExitTool{ctl: ctl, rev: rev}
}

func (t *ExitTool) Decl() llm.Tool {
	return llm.Tool{
		Name:        ExitToolName,
		Description: exitDescription,
		Parameters:  json.RawMessage(`{"type":"object","properties":{"plan":{"type":"string","description":"The complete plan, as markdown, starting with a # heading that names it."}},"required":["plan"],"additionalProperties":false}`),
	}
}

// Execute reviews the plan (dsh's flow): mode check, # heading
// validation, review round-trip; approval exits the mode, rejection
// carries the user's feedback back as an error result.
func (t *ExitTool) Execute(tc agent.ToolContext, args json.RawMessage) agent.ToolResult {
	var a struct {
		Plan string `json:"plan"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return agent.ErrorResult("invalid arguments: " + err.Error())
	}
	if !t.ctl.Active() {
		return agent.ErrorResult(ExitToolName + " is only available in plan mode")
	}
	trimmed := strings.TrimSpace(a.Plan)
	if !strings.HasPrefix(trimmed, "#") || !headingRe.MatchString(trimmed) {
		return agent.ErrorResult(ExitToolName + " requires a non-empty markdown plan starting with a # heading")
	}
	approved, feedback := t.rev.ReviewPlan(tc, a.Plan)
	if !approved {
		if feedback == "" {
			return agent.ErrorResult("The user chose to keep planning; revise the plan and present it again.")
		}
		return agent.ErrorResult("The user chose to keep planning; their feedback: " + feedback)
	}
	t.ctl.Set(false)
	return agent.TextResult("Plan approved — plan mode exited; carry out the plan starting with your next step.")
}

// headingRe is the # heading check (dsh validates /^#\s+\S/).
var headingRe = regexp.MustCompile(`^#\s+\S`)
