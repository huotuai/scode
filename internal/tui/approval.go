package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"scode/internal/agent"
	"scode/internal/cli"
	"scode/internal/llm"
	"scode/internal/permission"
)

// approvalRequest is one pending human decision rendered above the
// input; the answer channel carries the response back to the blocked
// agent goroutine.
type approvalRequest struct {
	kind   string // "tool" | "plan" | "sandbox"
	title  string
	body   string
	hint   string
	answer chan string
}

// approver implements cli.AskReviewer by routing asks into the
// bubbletea event stream and blocking until the model answers.
type approver struct {
	ui chan any
	mu sync.Mutex // serialize concurrent asks (parallel tool batches)
}

func newApprover(ui chan any) *approver { return &approver{ui: ui} }

// ask publishes the request and blocks until the model answers, the
// run's ctx cancels, or the program dies. "" means interrupted.
func (a *approver) ask(ctx context.Context, req *approvalRequest) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ui <- approvalMsg{req: req}
	select {
	case ans := <-req.answer:
		return ans
	case <-ctx.Done():
		return ""
	}
}

// approvalButton is one action of a tool/sandbox ask: Key is the
// answer letter the blocked agent goroutine understands, Label the
// button text.
type approvalButton struct {
	Key   string
	Label string
}

// approvalButtons lists an ask kind's actions in display order; the
// first button is the default focus. Plan reviews have their own
// actions (planButtons) and never route here.
func approvalButtons(kind string) []approvalButton {
	switch kind {
	case "tool":
		return []approvalButton{{"y", "允许一次"}, {"a", "本会话允许"}, {"p", "本项目允许"}, {"n", "拒绝"}}
	case "sandbox":
		return []approvalButton{{"y", "仅本次"}, {"a", "本会话生效"}, {"n", "拒绝"}}
	}
	return nil
}

func (a *approver) Ask(ctx context.Context, call llm.Block, rule *permission.Rule, exact string) cli.ApprovalResult {
	ans := a.ask(ctx, &approvalRequest{
		kind:   "tool",
		title:  fmt.Sprintf("工具审批 · 规则 %q", rule.Raw),
		body:   describeCall(call),
		hint:   "快捷键 [y] 允许一次 [a] 本会话允许 [p] 本项目允许 [n] 拒绝",
		answer: make(chan string, 1),
	})
	switch ans {
	case "y":
		return cli.ApprovalResult{Allow: true}
	case "a":
		return cli.ApprovalResult{Allow: true, SessionRule: exact}
	case "p":
		return cli.ApprovalResult{Allow: true, ProjectRule: exact}
	case "":
		return cli.ApprovalResult{Reason: "approval interrupted (run aborted)"}
	default:
		return cli.ApprovalResult{Reason: "denied by user"}
	}
}

// ReviewPlan: "y" approves (execution begins); any other text is
// feedback that sends the plan back for revision.
func (a *approver) ReviewPlan(tc agent.ToolContext, plan string) (bool, string) {
	ans := a.ask(tc.Ctx, &approvalRequest{
		kind:   "plan",
		title:  "plan review",
		body:   strings.TrimSpace(plan),
		hint:   "[y] 批准并执行  [n] 拒绝  其他输入=反馈意见,打回修订  [esc] 拒绝",
		answer: make(chan string, 1),
	})
	switch {
	case ans == "y":
		return true, ""
	case strings.HasPrefix(ans, "feedback:"):
		return false, strings.TrimPrefix(ans, "feedback:")
	case ans == "":
		return false, "the review was interrupted (run aborted); stay in plan mode and wait for the user's message"
	default:
		return false, ""
	}
}

func (a *approver) ReviewSandboxEscalation(ctx context.Context, req agent.EscalationRequest) agent.EscalationResult {
	ans := a.ask(ctx, &approvalRequest{
		kind:   "sandbox",
		title:  "沙箱提权审批",
		body:   fmt.Sprintf("%s  %s → %s\n%s\n理由: %s", req.Tool, req.CurrentMode, req.RequestedMode, req.Detail, req.Justification),
		hint:   "快捷键 [y] 仅本次 [a] 本会话生效 [n] 拒绝",
		answer: make(chan string, 1),
	})
	switch ans {
	case "y":
		return agent.EscalationResult{Approved: true}
	case "a":
		return agent.EscalationResult{Approved: true, ApplyToSession: true}
	case "":
		return agent.EscalationResult{Reason: "approval interrupted (run aborted)"}
	default:
		return agent.EscalationResult{Reason: "denied by user"}
	}
}

// describeCall renders the one-line call summary shown in the approval
// box: bash shows the command, file tools the path, everything else
// the tool name.
func describeCall(call llm.Block) string {
	kind, value := permission.CallSummary(call)
	if value == "" {
		return "  tool: " + call.Name
	}
	if len(value) > 400 {
		value = truncate(value, 400) // rune-safe: CJK survives the cut
	}
	return fmt.Sprintf("  %s %s: %s", call.Name, kind, value)
}
