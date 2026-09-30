package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"scode/internal/agent"
	"scode/internal/config"
	"scode/internal/llm"
	"scode/internal/permission"
	"scode/internal/planmode"
)

// Approver resolves "ask" verdicts from the permission engine. The
// interactive flavor prints a prompt on Err and waits for an answer
// routed from the REPL's input loop (Route); the non-interactive flavor
// (print mode, no stdin consumer) denies with an explanatory reason so
// unattended runs fail safe instead of hanging.
//
// Concurrent asks (parallel tool batches) serialize on mu: only one
// prompt is pending at a time, so the input loop can route lines
// unambiguously.
type Approver struct {
	Out         io.Writer
	Err         io.Writer
	interactive bool

	mu      sync.Mutex
	pending atomic.Bool
	kind    atomic.Value // "tool" | "plan": pending prompt kind (Route semantics differ); tool AND sandbox asks share "tool"
	answer  chan string
}

// NewApprover builds the approver for one app. interactive=true enables
// the y/n/a/p prompt; false auto-denies.
func NewApprover(interactive bool, out, errW io.Writer) *Approver {
	return &Approver{Out: out, Err: errW, interactive: interactive, answer: make(chan string, 1)}
}

// Pending reports whether an approval prompt awaits an answer (the
// REPL input loop consults this before steering).
func (a *Approver) Pending() bool { return a.pending.Load() }

// drainStaleAnswers discards any verdict left in the buffer by a
// previous ask that ended via cancellation after the user's line was
// already routed (Route consults Pending(), which the previous ask
// cleared only on its way out). Without the drain a stale "y"/"a"
// would silently approve a DIFFERENT prompt later. Callers hold mu and
// have not yet raised pending, so nothing new can arrive mid-drain.
func (a *Approver) drainStaleAnswers() {
	for {
		select {
		case <-a.answer:
		default:
			return
		}
	}
}

// Route feeds one input line into a pending approval. For tool
// approvals only y/n/a/p (case-insensitive) are consumed; anything else
// falls through to steering so mid-approval corrections still work. A
// pending PLAN review consumes every line: "y" approves, anything else
// is feedback that sends the plan back for revision.
func (a *Approver) Route(line string) bool {
	if !a.Pending() {
		return false
	}
	s := strings.ToLower(strings.TrimSpace(line))
	if a.kind.Load() == "plan" {
		if s == "y" || s == "yes" {
			a.answer <- "y"
		} else if s == "n" || s == "no" {
			a.answer <- ""
		} else {
			a.answer <- "feedback:" + strings.TrimSpace(line)
		}
		return true
	}
	switch s {
	case "y", "yes", "n", "no", "a", "always", "p", "project":
		a.answer <- s[:1]
		return true
	}
	return false
}

// ApprovalResult carries the user's verdict plus the rule to remember
// when the answer was 'a' (session) or 'p' (project).
type ApprovalResult struct {
	Allow       bool
	Reason      string // deny reason surfaced to the model
	SessionRule string // remember for the session ('a')
	ProjectRule string // persist to .scode/settings.json ('p')
}

// Ask presents one tool call for approval and blocks until the user
// answers or the run's ctx cancels.
func (a *Approver) Ask(ctx context.Context, call llm.Block, rule *permission.Rule, exact string) ApprovalResult {
	if !a.interactive {
		return ApprovalResult{
			Reason: fmt.Sprintf("rule %q requires approval, but this run is non-interactive; "+
				"add an allow rule to settings.json or .scode/settings.json to permit it", rule.Raw),
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.drainStaleAnswers()
	a.pending.Store(true)
	defer a.pending.Store(false)
	a.kind.Store("tool")

	fmt.Fprintf(a.Err, "\n── approval required (rule %q) ──\n%s\n[y] allow once  [n] deny  [a] always this session  [p] always for this project\n> ",
		rule.Raw, describeCall(call))
	for {
		select {
		case ans := <-a.answer:
			switch ans {
			case "y":
				return ApprovalResult{Allow: true}
			case "a":
				return ApprovalResult{Allow: true, SessionRule: exact}
			case "p":
				fmt.Fprintf(a.Err, "(rule saved to .scode/settings.json: %s)\n", exact)
				return ApprovalResult{Allow: true, ProjectRule: exact}
			default:
				return ApprovalResult{Reason: "denied by user"}
			}
		case <-ctx.Done():
			return ApprovalResult{Reason: "approval interrupted (run aborted)"}
		}
	}
}

// ReviewPlan implements planmode.Reviewer: the completed plan is shown
// in full; "y" approves (plan mode exits, execution begins), any other
// line sends the plan back with that line as feedback (the "指导"
// channel — dsh's keep-planning with custom text).
func (a *Approver) ReviewPlan(tc agent.ToolContext, plan string) (bool, string) {
	if !a.interactive {
		return false, "no interactive review channel is available; ask the user to run /plan off to leave plan mode"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.drainStaleAnswers()
	a.pending.Store(true)
	defer a.pending.Store(false)
	a.kind.Store("plan")

	fmt.Fprintf(a.Out, "\n── plan review ──\n%s\n", strings.TrimSpace(plan))
	fmt.Fprint(a.Err, "[y] approve and start executing — anything else sends the plan back with your words as feedback\n> ")
	select {
	case ans := <-a.answer:
		switch {
		case ans == "y":
			return true, ""
		case strings.HasPrefix(ans, "feedback:"):
			return false, strings.TrimPrefix(ans, "feedback:")
		default:
			return false, ""
		}
	case <-tc.Ctx.Done():
		return false, "the review was interrupted (run aborted); stay in plan mode and wait for the user's message"
	}
}

// ReviewSandboxEscalation presents one sandbox widening for approval
// (y = this call only, a = switch the session mode, anything else = deny).
func (a *Approver) ReviewSandboxEscalation(ctx context.Context, req agent.EscalationRequest) agent.EscalationResult {
	if !a.interactive {
		return agent.EscalationResult{
			Reason: "sandbox escalation requires approval, but this run is non-interactive; " +
				"ask the user to raise the sandbox mode (session/sandbox or settings.json sandbox.mode)",
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.drainStaleAnswers()
	a.pending.Store(true)
	defer a.pending.Store(false)
	// Route's tool semantics cover this prompt's answers (y/a/n);
	// without the store a stale "plan" from an earlier review would
	// rewrite "a" into feedback and silently deny the session widening.
	a.kind.Store("tool")

	fmt.Fprintf(a.Err, "\n── sandbox escalation ──\n%s  %s → %s\n%s\n理由:%s\n[y] 仅本次  [a] 本会话生效  [n] 拒绝\n> ",
		req.Tool, req.CurrentMode, req.RequestedMode, req.Detail, req.Justification)
	for {
		select {
		case ans := <-a.answer:
			switch ans {
			case "y":
				return agent.EscalationResult{Approved: true}
			case "a":
				return agent.EscalationResult{Approved: true, ApplyToSession: true}
			default:
				return agent.EscalationResult{Reason: "denied by user"}
			}
		case <-ctx.Done():
			return agent.EscalationResult{Reason: "approval interrupted (run aborted)"}
		}
	}
}

// describeCall renders the one-line call summary shown in the approval
// prompt: bash shows the command, file tools the path, everything else
// the tool name.
func describeCall(call llm.Block) string {
	kind, value := permission.CallSummary(call)
	if value == "" {
		return "  tool: " + call.Name
	}
	if len(value) > 200 {
		r := []rune(value) // byte cutting would split a CJK rune
		value = string(r[:200]) + "…"
	}
	return fmt.Sprintf("  %s %s: %s", call.Name, kind, value)
}

// AskReviewer is the approval seam shared by the Before hook and the
// plan exit tool: the CLI Approver (terminal prompts) and the serve
// transport (JSON-RPC reverse requests) both implement it.
type AskReviewer interface {
	Ask(ctx context.Context, call llm.Block, rule *permission.Rule, exact string) ApprovalResult
	ReviewPlan(tc agent.ToolContext, plan string) (approved bool, feedback string)
	// ReviewSandboxEscalation asks the human to widen one call's sandbox
	// mode (dsh's approveEscalation channel).
	ReviewSandboxEscalation(ctx context.Context, req agent.EscalationRequest) agent.EscalationResult
}

// beforeHook builds the agent's BeforeToolCall from the engine and
// approver (design: interception lives in the permission layer, tools
// stay registered; a deny is a readable tool result the model can
// react to).
func beforeHook(engine *permission.Engine, ar AskReviewer, cwd string, planCtl *planmode.Controller) agent.BeforeToolCall {
	return func(ctx context.Context, call llm.Block) (bool, string) {
		// Plan mode's built-in guard evaluates FIRST: the read-only
		// promise is a mode guarantee that configured allow rules must
		// not soften (deny rules only ever restrict further).
		if planCtl != nil && planCtl.Active() {
			if blocked, reason := planmode.Deny(call, cwd); blocked {
				return true, reason
			}
		}
		dec, rule := engine.Evaluate(call)
		switch dec {
		case permission.Deny:
			return true, fmt.Sprintf("permission denied by rule %q; the user has not permitted this operation", rule.Raw)
		case permission.Ask:
			res := ar.Ask(ctx, call, rule, permission.ExactRule(cwd, call))
			if res.Allow {
				if res.SessionRule != "" {
					engine.AllowForSession(res.SessionRule)
				}
				if res.ProjectRule != "" {
					if err := config.AddProjectAllowRule(cwd, res.ProjectRule); err == nil {
						_ = engine.AddAllow(res.ProjectRule)
					}
				}
				return false, ""
			}
			return true, res.Reason
		default:
			return false, ""
		}
	}
}
