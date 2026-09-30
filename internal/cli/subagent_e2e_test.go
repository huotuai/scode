package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"scode/internal/llm"
	"scode/internal/subagent"
)

// E2E: the parent model delegates through the task tool; the delegate
// runs ISOLATED (its request carries only the read-only tools), its
// report returns as the tool result, and its usage folds into the
// session spend.
func TestSubAgentE2E(t *testing.T) {
	var calls int32
	var subSeen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hasTask := strings.Contains(string(body), `"task"`)
		n := atomic.AddInt32(&calls, 1)
		switch {
		case hasTask && n == 1: // parent turn 1: delegate
			chatSSEToolCall(w, "task", `{"agent":"researcher","prompt":"找出 emit 的定义"}`)
		case !hasTask: // the delegate's own loop
			subSeen = true
			if strings.Contains(string(body), `"edit"`) || strings.Contains(string(body), `"write"`) || strings.Contains(string(body), `"bash"`) {
				t.Errorf("delegate carries a mutating tool:\n%s", string(body))
			}
			chatSSE(w, "emit 定义在 loop.go:233", 50)
		default: // parent turn 2: wrap up
			chatSSE(w, "委托完成", 10)
		}
	}))
	defer srv.Close()
	setupTestApp(t, srv)

	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck

	if err := app.AgentSave(subagent.Spec{Name: "researcher", Description: "代码考古", Effort: "low"}); err != nil {
		t.Fatal(err)
	}
	if listing := app.AgentListText(); !strings.Contains(listing, "researcher") || !strings.Contains(listing, "继承会话模型") {
		t.Fatalf("listing missing the agent:\n%s", listing)
	}
	if listing := app.AgentListText(); !strings.Contains(listing, "general-purpose (内置)") {
		t.Fatalf("listing missing the shipped built-in:\n%s", listing)
	}

	r := &Renderer{Out: io.Discard, Err: io.Discard}
	if err := runPrompt(context.Background(), app, r, "帮我委派调研"); err != nil {
		t.Fatal(err)
	}
	if !subSeen {
		t.Fatal("the delegate never ran")
	}
	if calls < 3 {
		t.Fatalf("provider calls = %d, want ≥3 (delegate + two parent turns)", calls)
	}

	// The task result rode back into the conversation.
	msgs := app.Tr.Messages()
	var result string
	for i := len(msgs) - 1; i >= 0; i-- {
		for _, b := range msgs[i].Content {
			if b.Kind != llm.BlockToolResult {
				continue
			}
			for _, nb := range b.Content {
				if strings.Contains(nb.Text, "loop.go:233") {
					result = nb.Text
				}
			}
		}
	}
	if !strings.Contains(result, "(sub-agent researcher ·") {
		t.Fatalf("task result missing the delegate footer:\n%s", result)
	}

	// The delegate's usage folded into the session accounting.
	if u := app.UsageReport(); u.Input < 60 {
		t.Fatalf("delegate usage not folded: input=%d", u.Input)
	}
}
