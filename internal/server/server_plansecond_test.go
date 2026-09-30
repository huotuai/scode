package server

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The desktop's plan badge/panel polls session/plan: a SECOND plan in
// the same session must replace the first (whole-value replace).
func TestServerPlanSecondPlanE2E(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			chatSSEToolCall(w, "update_plan", `{"plan":[{"step":"甲","status":"completed"},{"step":"乙","status":"completed"}]}`)
		case 3:
			chatSSEToolCall(w, "update_plan", `{"plan":[{"step":"一","status":"in_progress"},{"step":"二","status":"pending"},{"step":"三","status":"pending"},{"step":"四","status":"pending"}]}`)
		default:
			chatSSE(w, "done")
		}
	}))
	defer srv.Close()
	testEnv(t, srv, nil)

	client := startPair(t, nil)
	created := callRPC(t, client, "session/create", nil)
	id := created["sessionId"].(string)

	// waitNewAgentEnd waits for an agent_end that arrives AFTER the call
	// (waitEvent alone would match the previous run's agent_end).
	waitNewAgentEnd := func() {
		client.mu.Lock()
		from := len(client.events)
		client.mu.Unlock()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			client.mu.Lock()
			for _, e := range client.events[from:] {
				if e["type"] == "agent_end" {
					client.mu.Unlock()
					return
				}
			}
			client.mu.Unlock()
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("agent_end for the new run never arrived")
	}

	planOf := func() map[string]any {
		res := callRPC(t, client, "session/plan", map[string]any{"sessionId": id})
		plan, _ := res["plan"].(map[string]any)
		if plan == nil {
			t.Fatalf("session/plan returned no plan: %v", res)
		}
		return plan
	}

	callRPC(t, client, "session/prompt", map[string]any{"sessionId": id, "text": "first task"})
	waitNewAgentEnd()
	if items := planOf()["items"].([]any); len(items) != 2 {
		t.Fatalf("first plan items = %v, want 2", items)
	}

	callRPC(t, client, "session/prompt", map[string]any{"sessionId": id, "text": "second task"})
	waitNewAgentEnd()

	items := planOf()["items"].([]any)
	if len(items) != 4 {
		t.Fatalf("second plan did not replace the first over session/plan: %v", items)
	}
	first := items[0].(map[string]any)
	if first["step"] != "一" || first["status"] != "in_progress" {
		t.Fatalf("second plan first item = %v", first)
	}
}
