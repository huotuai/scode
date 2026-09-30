package tui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"

	"scode/internal/plantrack"
	"scode/internal/session"
)

// chatSSEPlanCall answers request 1 with an update_plan tool call (the
// real tool executes against the app's tracker); later requests stream
// plain text.
func chatSSEPlanCall(w http.ResponseWriter, args string) {
	w.Header().Set("content-type", "text/event-stream")
	ab, _ := json.Marshal(args)
	chunks := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"update_plan","arguments":` + string(ab) + `}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		`[DONE]`,
	}
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
}

// The checklist block: header counts + current step, ✔/◐/○ markers,
// completed struck through, in-progress accented.
func TestPlanBlockRendering(t *testing.T) {
	plan := session.PlanEntry{
		Explanation: "开始修复流程",
		Items: []session.PlanItem{
			{Step: "分析代码库", Status: plantrack.Completed},
			{Step: "修复登录逻辑", Status: plantrack.InProgress},
			{Step: "运行测试", Status: plantrack.Pending},
		},
	}
	raw := planBlock(plan, 80)
	view := stripANSI(raw)
	for _, want := range []string{
		"计划 1/3 · 修复登录逻辑", // done-count + current step
		"✔ 分析代码库",
		"◐ 修复登录逻辑",
		"○ 运行测试",
		"开始修复流程",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("plan block missing %q:\n%s", want, view)
		}
	}
	// Completed steps strike through (SGR 9 rides the style's parameter
	// list, e.g. "\x1b[90;9m…").
	if !strings.Contains(raw, ";9m") {
		t.Fatal("completed step not struck through:\n" + raw)
	}
	// Item rows align on the gutter text column.
	lines := strings.Split(view, "\n")
	for _, l := range lines[1:] {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if !strings.HasPrefix(l, gutterPad) {
			t.Fatalf("item row not gutter-indented: %q", l)
		}
	}
}

// E2E: the agent calls update_plan through the real tool registry —
// the tracker commits, the TUI renders the checklist block after the
// tool row, the status bar carries the progress count, and a fresh
// model on the same session replays the plan at startup.
func TestPlanProgressE2E(t *testing.T) {
	var calls int32
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			chatSSEPlanCall(w, `{"explanation":"开始","plan":[`+
				`{"step":"分析代码库","status":"completed"},`+
				`{"step":"修复登录逻辑","status":"in_progress"}]}`)
			return
		}
		chatSSE(w, "done")
	})
	ui := make(chan any, 256)
	m := newModel(app, ui)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(model)

	m.input.SetValue("plan it")
	tm, _ = m.submit()
	m = tm.(model)
	pumpUntil(t, &m, ui, func(msg any) bool {
		_, ok := msg.(runDoneMsg)
		return ok
	})

	txt := blocksText(&m)
	if !strings.Contains(txt, "计划 1/2") || !strings.Contains(txt, "◐ 修复登录逻辑") {
		t.Fatalf("checklist block missing after update_plan ran:\n%s", txt)
	}

	// The status bar carries the progress count.
	segs := m.statusSegments()
	joined := joinStatusSegs(segs)
	if !strings.Contains(stripANSI(joined), "计划 1/2") {
		t.Fatalf("status bar missing plan progress:\n%s", stripANSI(joined))
	}

	// A fresh model on the same session replays the plan at startup.
	m2 := newModel(app, make(chan any, 4))
	if txt2 := blocksText(&m2); !strings.Contains(txt2, "计划 1/2") {
		t.Fatalf("startup replay missing the plan:\n%s", truncate(txt2, 400))
	}
}
