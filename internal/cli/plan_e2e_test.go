package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"scode/internal/agent"
)

// Plan mode end-to-end: --plan starts read-only, a write call is denied
// with guidance, the mode entry lands on disk, and resume restores the
// active state.
func TestPlanModeDenyAndResumeE2E(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			chatSSEToolCall(w, "write", `{"path":"main.go","content":"package main"}`)
			return
		}
		chatSSE(w, "right, planning only", 10)
	}))
	defer srv.Close()
	setupTestApp(t, srv)

	app, err := Setup(Options{Plan: true})
	if err != nil {
		t.Fatal(err)
	}
	if !app.planCtl.Active() {
		t.Fatal("--plan should start in plan mode")
	}

	out := make(chan agent.Event, 256)
	var toolErr string
	done := make(chan error, 1)
	go func() { done <- app.Run(context.Background(), out, "rewrite main.go") }()
	for ev := range out {
		if ev.Type == agent.EvToolEnd && ev.Result != nil && ev.Result.IsError {
			toolErr = ev.Result.Content[0].Text
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(toolErr, "plan mode is read-only") {
		t.Fatalf("write should be denied in plan mode, got %q", toolErr)
	}

	id := app.Sess.Header().ID
	if got := currentModeOf(app); got != "plan" {
		t.Fatalf("mode entry should persist as plan, got %q", got)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}

	// Resume replays the mode entry: plan mode is active again.
	app2, err := Setup(Options{Resume: id})
	if err != nil {
		t.Fatal(err)
	}
	defer app2.Close() //nolint:errcheck
	if !app2.planCtl.Active() {
		t.Fatal("resume should restore plan mode")
	}
}

// Plan approval end-to-end: the model submits exit_plan_mode, the user
// approves, and the mode flips to default mid-run.
func TestPlanApprovalExitsE2E(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			chatSSEToolCall(w, "exit_plan_mode", `{"plan":"# Add feature\n\n1. step one"}`)
			return
		}
		chatSSE(w, "executing now", 10)
	}))
	defer srv.Close()
	setupTestApp(t, srv)

	app, err := Setup(Options{Plan: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck
	app.approver.interactive = true
	// Answer the review as soon as it pends (the REPL input loop's role).
	go func() {
		for !app.approver.Pending() {
			time.Sleep(time.Millisecond)
		}
		app.approver.Route("y")
	}()

	out := make(chan agent.Event, 256)
	done := make(chan error, 1)
	go func() { done <- app.Run(context.Background(), out, "plan the feature") }()
	for range out {
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if app.planCtl.Active() {
		t.Fatal("approval should exit plan mode")
	}
	if got := currentModeOf(app); got != "default" {
		t.Fatalf("mode entry should persist as default, got %q", got)
	}
}

// currentModeOf folds the session's mode entries (mirrors the on-disk state).
func currentModeOf(app *App) string {
	m := ""
	for _, e := range app.entries {
		if e.Mode != nil {
			m = e.Mode.Mode
		}
	}
	return m
}
