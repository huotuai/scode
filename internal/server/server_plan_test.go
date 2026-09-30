package server

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Plan approval over the serve RPC path the desktop uses: session/mode
// flips to plan, the model submits exit_plan_mode, and the client's
// approval/request carries kind=plan.
func TestServerPlanApprovalE2E(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			chatSSEToolCall(w, "exit_plan_mode", `{"plan":"# Add feature\n\n1. step one"}`)
			return
		}
		chatSSE(w, "executing now")
	}))
	defer srv.Close()
	testEnv(t, srv, nil)

	client := startPair(t, &approvalResponse{Decision: "approve"})

	created := callRPC(t, client, "session/create", nil)
	id := created["sessionId"].(string)
	st := callRPC(t, client, "session/mode", map[string]any{"sessionId": id, "mode": "plan"})
	if st["mode"] != "plan" {
		t.Fatalf("mode after switch = %v, want plan", st["mode"])
	}

	callRPC(t, client, "session/prompt", map[string]any{"sessionId": id, "text": "plan the feature"})
	client.waitEvent(t, "agent_end")

	client.mu.Lock()
	var sawPlan bool
	for _, r := range client.gotReq {
		if r["method"] == "approval/request" {
			p, _ := r["params"].(map[string]any)
			if p["kind"] == "plan" {
				sawPlan = true
				if p["sessionId"] != id {
					t.Fatalf("plan approval sessionId = %v, want %v", p["sessionId"], id)
				}
			}
		}
	}
	client.mu.Unlock()
	if !sawPlan {
		t.Fatalf("client never received a plan approval/request; got %v", client.gotReq)
	}

	st = callRPC(t, client, "session/state", map[string]any{"sessionId": id})
	if st["mode"] != "default" {
		t.Fatalf("mode after approval = %v, want default", st["mode"])
	}
}
