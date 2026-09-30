package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"scode/internal/agent"
	"scode/internal/sandbox"
)

// Resolve absolutizes a tool path against the session working directory.
func Resolve(tc agent.ToolContext, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	base := tc.CWD
	if base == "" {
		if wd, err := os.Getwd(); err == nil {
			base = wd
		}
	}
	return filepath.Join(base, path)
}

// FenceWrite applies the sandbox file-effect policy to a mutation target
// (dsh's fs-sandbox fence): the return value is the EXACT canonical path
// the mutation must use, so the checked identity is the mutated one.
// A refusal is a readable tool error carrying the model-facing denial
// marker — the model self-corrects or escalates.
func FenceWrite(p sandbox.Policy, tc agent.ToolContext, displayPath string) (string, error) {
	return p.CheckWrite(Resolve(tc, displayPath))
}

// SandboxError renders a policy refusal as the tool error text with the
// model-facing marker pair (denial + escalation hint).
func SandboxError(err error, subject string) agent.ToolResult {
	var d *sandbox.DenialError
	if errors.As(err, &d) {
		return agent.ErrorResult(fmt.Sprintf("%s\n%s\n%s", err.Error(), sandbox.DenialMarker(d.Mode), sandbox.EscalationHint(subject)))
	}
	return agent.ErrorResult(err.Error())
}

// escalationArgs carries the sandbox escalation field pair parsed from a
// tool call (sandbox_permissions + justification travel together).
type escalationArgs struct {
	permissions   string
	justification string
}

// ResolvePolicy resolves the file-effect policy for ONE tool call (dsh's
// approveEscalation choreography): the session's standing policy; a
// sandbox_permissions request is judged (pairing validation, strictly-
// wider ladder) and, when widening, approved through the session's
// escalation channel before anything executes. detail is the command or
// path shown in the approval prompt: the tools pass an ABSOLUTE path (or the
// verbatim command), and ResolvePolicy sanitizes it into one bounded line
// here — this is the single producer of escalation prompts, so every current
// and future tool inherits the same disclosure discipline.
//
// Fail closed: a call with no composed policy, or with an unresolved mode,
// is refused instead of running unfenced — absence of a sandbox is a wiring
// bug, never a permission.
func ResolvePolicy(tc agent.ToolContext, permissions, justification, detail string) (sandbox.Policy, *agent.ToolResult) {
	if tc.Sandbox == nil {
		r := agent.ErrorResult("sandbox: no policy is composed for this session, so this call is refused rather than run unfenced (fail-closed). The tool wiring must supply the session's sandbox mode.")
		return sandbox.Policy{}, &r
	}
	p := sandbox.Policy{Mode: sandbox.Mode(tc.Sandbox.Mode), WorkspaceRoot: tc.CWD}
	if tc.Sandbox.WorkspaceRoot != "" {
		p.WorkspaceRoot = tc.Sandbox.WorkspaceRoot
	}
	if !p.Mode.Valid() {
		r := agent.ErrorResult(fmt.Sprintf("sandbox: unresolved mode %q for this session, so this call is refused rather than run unfenced (fail-closed)", p.Mode))
		return p, &r
	}
	if err := sandbox.ValidateEscalationArgs(permissions, justification); err != nil {
		r := agent.ErrorResult(err.Error())
		return p, &r
	}
	if permissions == "" {
		return p, nil // proceed under the standing policy
	}
	target, needsApproval, err := sandbox.JudgeEscalation(permissions, p.Mode)
	if err != nil {
		r := agent.ErrorResult(err.Error())
		return p, &r
	}
	if !needsApproval {
		return p, nil
	}
	if tc.Escalate == nil {
		r := agent.ErrorResult("sandbox escalation unavailable: no approval channel is composed")
		return p, &r
	}
	// Both fields land verbatim in the approval card, so both get the same
	// bounded, layout-safe rendering; the justification is "one sentence" by
	// contract, so the bound is generous rather than restrictive.
	res := tc.Escalate(permissions, sandbox.SummarizeDetail(justification), sandbox.SummarizeDetail(detail))
	if !res.Approved {
		reason := res.Reason
		if reason == "" {
			reason = "the user rejected the escalation"
		}
		r := agent.ErrorResult(fmt.Sprintf("sandbox escalation rejected: %s", reason))
		return p, &r
	}
	p.Mode = target
	return p, nil
}

// mutationLocks serializes file writes per absolute path so concurrent
// tool calls in one turn cannot interleave writes to the same file
// (pi's file-mutation-queue).
var mutationLocks sync.Map // map[string]*sync.Mutex

func WithFileMutation(path string, fn func() error) error {
	mAny, _ := mutationLocks.LoadOrStore(path, &sync.Mutex{})
	m := mAny.(*sync.Mutex)
	m.Lock()
	defer m.Unlock()
	return fn()
}
