// Plan progress regression tests: a second plan (or a second progress
// report) in the same session must refresh the status bar — the bar
// reads the tracker live on every render.
package tui

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"

	"scode/internal/cli"
)

func TestPlanProgressSecondPlanE2E(t *testing.T) {
	var calls int32
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			chatSSEPlanCall(w, `{"plan":[`+
				`{"step":"甲","status":"completed"},`+
				`{"step":"乙","status":"completed"}]}`)
		case 3:
			chatSSEPlanCall(w, `{"plan":[`+
				`{"step":"一","status":"in_progress"},`+
				`{"step":"二","status":"pending"},`+
				`{"step":"三","status":"pending"},`+
				`{"step":"四","status":"pending"}]}`)
		default:
			chatSSE(w, "done")
		}
	})
	ui := make(chan any, 256)
	m := newModel(app, ui)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(model)

	run := func(prompt string) {
		m.input.SetValue(prompt)
		tm, _ := m.submit()
		m = tm.(model)
		pumpUntil(t, &m, ui, func(msg any) bool {
			_, ok := msg.(runDoneMsg)
			return ok
		})
	}
	run("first task")
	if bar := stripANSI(joinStatusSegs(m.statusSegments())); !strings.Contains(bar, "计划 2/2") {
		t.Fatalf("first plan missing from status bar:\n%s", bar)
	}
	run("second task")
	bar := stripANSI(joinStatusSegs(m.statusSegments()))
	if !strings.Contains(bar, "计划 0/4") {
		t.Fatalf("second plan did not refresh the status bar:\n%s", bar)
	}
	if strings.Contains(bar, "计划 2/2") {
		t.Fatalf("status bar still shows the first plan:\n%s", bar)
	}
}

// Same run, two update_plan calls: the progress update must move the
// status bar (0/4 -> 1/4), not stick at the first call's numbers.
func TestPlanProgressWithinRunE2E(t *testing.T) {
	var calls int32
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			chatSSEPlanCall(w, `{"plan":[`+
				`{"step":"一","status":"in_progress"},`+
				`{"step":"二","status":"pending"},`+
				`{"step":"三","status":"pending"},`+
				`{"step":"四","status":"pending"}]}`)
		case 2:
			chatSSEPlanCall(w, `{"plan":[`+
				`{"step":"一","status":"completed"},`+
				`{"step":"二","status":"in_progress"},`+
				`{"step":"三","status":"pending"},`+
				`{"step":"四","status":"pending"}]}`)
		default:
			chatSSE(w, "done")
		}
	})
	ui := make(chan any, 256)
	m := newModel(app, ui)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(model)

	m.input.SetValue("task")
	tm, _ = m.submit()
	m = tm.(model)
	pumpUntil(t, &m, ui, func(msg any) bool {
		_, ok := msg.(runDoneMsg)
		return ok
	})
	bar := stripANSI(joinStatusSegs(m.statusSegments()))
	if !strings.Contains(bar, "计划 1/4") {
		t.Fatalf("second update_plan within one run did not move the status bar:\n%s", bar)
	}
}

// Resume: the first plan replays from the log, the SECOND plan (after
// the restart) must still move the status bar.
func TestPlanProgressAfterResumeE2E(t *testing.T) {
	var calls int32
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			chatSSEPlanCall(w, `{"plan":[{"step":"甲","status":"completed"},{"step":"乙","status":"completed"}]}`)
		case 3:
			chatSSEPlanCall(w, `{"plan":[{"step":"一","status":"in_progress"},{"step":"二","status":"pending"}]}`)
		default:
			chatSSE(w, "done")
		}
	})
	ui := make(chan any, 256)
	m := newModel(app, ui)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(model)
	run := func(prompt string) {
		m.input.SetValue(prompt)
		tm, _ := m.submit()
		m = tm.(model)
		pumpUntil(t, &m, ui, func(msg any) bool {
			_, ok := msg.(runDoneMsg)
			return ok
		})
	}
	run("first task")
	if bar := stripANSI(joinStatusSegs(m.statusSegments())); !strings.Contains(bar, "计划 2/2") {
		t.Fatalf("first plan missing:\n%s", bar)
	}
	id := app.Sess.Header().ID
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}

	app2, err := cli.Setup(cli.Options{Resume: id})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app2.Close() }) //nolint:errcheck
	m2 := newModel(app2, ui)
	tm, _ = m2.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m2 = tm.(model)
	if bar := stripANSI(joinStatusSegs(m2.statusSegments())); !strings.Contains(bar, "计划 2/2") {
		t.Fatalf("resumed plan missing:\n%s", bar)
	}
	m2.input.SetValue("second task")
	tm, _ = m2.submit()
	m2 = tm.(model)
	pumpUntil(t, &m2, ui, func(msg any) bool {
		_, ok := msg.(runDoneMsg)
		return ok
	})
	bar := stripANSI(joinStatusSegs(m2.statusSegments()))
	if !strings.Contains(bar, "计划 0/2") {
		t.Fatalf("second plan after resume did not move the status bar:\n%s", bar)
	}
}
