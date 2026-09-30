package cli

import (
	"context"
	"io"
	"testing"
	"time"

	"scode/internal/agent"
	"scode/internal/llm"
	"scode/internal/permission"
)

func askRule(t *testing.T, raw string) *permission.Rule {
	t.Helper()
	r, err := permission.ParseRule(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &r
}

func testCall() llm.Block {
	return llm.Block{Kind: llm.BlockToolCall, Name: "bash", Arguments: []byte(`{"command":"go test ./..."}`)}
}

func TestApproverNonInteractiveDenies(t *testing.T) {
	a := NewApprover(false, io.Discard, io.Discard)
	res := a.Ask(context.Background(), testCall(), askRule(t, "bash(*)"), "bash(go test ./...)")
	if res.Allow || res.Reason == "" {
		t.Fatalf("non-interactive ask must deny with a reason: %+v", res)
	}
}

func TestApproverInteractiveAnswers(t *testing.T) {
	cases := []struct {
		line        string
		wantAllow   bool
		wantSession bool
		wantProject bool
	}{
		{"y", true, false, false},
		{"yes", true, false, false},
		{"a", true, true, false},
		{"p", true, false, true},
		{"n", false, false, false},
	}
	for _, c := range cases {
		t.Run(c.line, func(t *testing.T) {
			a := NewApprover(true, io.Discard, io.Discard)
			resCh := make(chan ApprovalResult, 1)
			go func() { resCh <- a.Ask(context.Background(), testCall(), askRule(t, "bash(*)"), "bash(go test ./...)") }()
			deadline := time.Now().Add(2 * time.Second)
			for !a.Pending() {
				if time.Now().After(deadline) {
					t.Fatal("prompt never went pending")
				}
				time.Sleep(time.Millisecond)
			}
			if !a.Route(c.line) {
				t.Fatalf("Route(%q) not consumed", c.line)
			}
			res := <-resCh
			if res.Allow != c.wantAllow {
				t.Errorf("allow=%v want %v", res.Allow, c.wantAllow)
			}
			if (res.SessionRule != "") != c.wantSession {
				t.Errorf("sessionRule=%q", res.SessionRule)
			}
			if (res.ProjectRule != "") != c.wantProject {
				t.Errorf("projectRule=%q", res.ProjectRule)
			}
		})
	}
}

func TestApproverUnrelatedLinesFallThrough(t *testing.T) {
	a := NewApprover(true, io.Discard, io.Discard)
	if a.Route("keep going") {
		t.Fatal("no pending prompt: line must not be consumed")
	}
	resCh := make(chan ApprovalResult, 1)
	go func() { resCh <- a.Ask(context.Background(), testCall(), askRule(t, "bash(*)"), "x") }()
	for !a.Pending() {
		time.Sleep(time.Millisecond)
	}
	if a.Route("keep going") { // steering stays possible mid-approval
		t.Fatal("unrelated line consumed by approval")
	}
	a.Route("y")
	if res := <-resCh; !res.Allow {
		t.Fatal("expected allow")
	}
}

func TestApproverCancelUnblocks(t *testing.T) {
	a := NewApprover(true, io.Discard, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	resCh := make(chan ApprovalResult, 1)
	go func() { resCh <- a.Ask(ctx, testCall(), askRule(t, "bash(*)"), "x") }()
	for !a.Pending() {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case res := <-resCh:
		if res.Allow {
			t.Fatal("cancelled approval must deny")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled approval stayed blocked — would strand the tool goroutine")
	}
}

func TestBeforeHookDenyShortCircuits(t *testing.T) {
	engine, err := permission.New("", "", nil, nil, []string{`bash(rm:*)`})
	if err != nil {
		t.Fatal(err)
	}
	// A non-interactive approver would deny anyway; the point is the
	// hook never reaches it for a deny verdict.
	hook := beforeHook(engine, NewApprover(false, io.Discard, io.Discard), "", nil)
	blocked, reason := hook(context.Background(), llm.Block{
		Kind: llm.BlockToolCall, Name: "bash", Arguments: []byte(`{"command":"rm -rf build"}`),
	})
	if !blocked || reason == "" {
		t.Fatalf("deny rule must block with a reason: %v %q", blocked, reason)
	}
	blocked, _ = hook(context.Background(), llm.Block{
		Kind: llm.BlockToolCall, Name: "bash", Arguments: []byte(`{"command":"ls"}`),
	})
	if blocked {
		t.Fatal("unmatched call must pass")
	}
}

func waitPending(t *testing.T, a *Approver) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !a.Pending() {
		if time.Now().After(deadline) {
			t.Fatal("prompt never went pending")
		}
		time.Sleep(time.Millisecond)
	}
}

// A plan review must not leak its "plan" routing semantics into a later
// sandbox escalation: Route would rewrite the advertised "a" into
// feedback and silently deny the session widening the prompt offers.
func TestSandboxEscalationAfterPlanReview(t *testing.T) {
	a := NewApprover(true, io.Discard, io.Discard)

	planCh := make(chan bool, 1)
	go func() {
		ok, _ := a.ReviewPlan(agent.ToolContext{Ctx: context.Background()}, "the plan")
		planCh <- ok
	}()
	waitPending(t, a)
	if !a.Route("y") {
		t.Fatal("plan y not consumed")
	}
	if ok := <-planCh; !ok {
		t.Fatal("plan not approved")
	}

	escCh := make(chan agent.EscalationResult, 1)
	go func() {
		escCh <- a.ReviewSandboxEscalation(context.Background(), agent.EscalationRequest{
			Tool: "bash", CurrentMode: "read-only", RequestedMode: "workspace-write", Justification: "need to write",
		})
	}()
	waitPending(t, a)
	if !a.Route("a") {
		t.Fatal("escalation a not consumed")
	}
	res := <-escCh
	if !res.Approved || !res.ApplyToSession {
		t.Fatalf("escalation result = %+v, want session-wide approval", res)
	}
}

// A verdict routed just as its ask was cancelled must not linger in the
// answer buffer and silently approve a LATER, different approval.
func TestStaleAnswerDrainedOnNewAsk(t *testing.T) {
	a := NewApprover(true, io.Discard, io.Discard)

	// Simulate the tail of an interrupted ask: the user's "y" was routed
	// (pending still true at Route time), then the run aborted before
	// the ask's select consumed it.
	a.pending.Store(true)
	if !a.Route("y") {
		t.Fatal("y not consumed")
	}
	a.pending.Store(false)

	resCh := make(chan ApprovalResult, 1)
	go func() {
		resCh <- a.Ask(context.Background(), testCall(), askRule(t, "bash(*)"), "bash(go test ./...)")
	}()
	waitPending(t, a)
	select {
	case r := <-resCh:
		t.Fatalf("stale verdict auto-answered the new ask: %+v", r)
	case <-time.After(150 * time.Millisecond):
		// Still blocked: the stale y was drained on entry — correct.
	}
	// A real answer still works.
	if !a.Route("n") {
		t.Fatal("n not consumed")
	}
	if r := <-resCh; r.Allow {
		t.Fatalf("real answer lost: %+v", r)
	}
}
