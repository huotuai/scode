package tui

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"scode/internal/agent"
	"scode/internal/llm"
	"scode/internal/subagent"
)

// The /agents overlay flow: open, create an agent through the form
// (model + effort), edit it, delete it — every save persists to
// agents.json immediately and takes effect on the next delegation.
func TestAgentsOverlayFlow(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	m := newModel(app, make(chan any, 16))
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(model)

	m.runCommand("/agents")
	if !m.agentsOpen || m.agentsStage != agentsStageList {
		t.Fatal("overlay did not open on the list")
	}
	// The merged list ALWAYS carries the shipped built-ins, each with
	// the read-only toolset badge.
	if view := plain(m.agentsView()); !strings.Contains(view, "general-purpose") ||
		!strings.Contains(view, "explore") || !strings.Contains(view, "内置") ||
		!strings.Contains(view, "4 个工具") {
		t.Fatalf("built-in agents missing from the empty list:\n%s", view)
	}
	// The modal hides the input box.
	if !m.inputHidden() {
		t.Fatal("/agents should hide the input box")
	}

	// A pure built-in cannot be deleted — d guards with a toast.
	m = press(t, m, 'd')
	if m.agentsConfirmDel || m.toast == nil || !strings.Contains(m.toast.text, "内置代理不可删除") {
		t.Fatalf("builtin delete not guarded: confirm=%v toast=%+v", m.agentsConfirmDel, m.toast)
	}
	if _, err := app.Agents(); err != nil || len(m.agentsSpecs) == 0 {
		t.Fatalf("guard should not touch anything: specs=%v err=%v", m.agentsSpecs, err)
	}

	// n → the form: name, description, model (cycled from the ACTIVATED
	// models), effort, save.
	m = press(t, m, 'n')
	if m.agentsStage != agentsStageEdit {
		t.Fatal("n did not open the form")
	}
	if len(m.agentModels) == 0 {
		t.Fatal("form has no activated models to pick from")
	}
	m = typeText(t, m, "researcher")
	m = press(t, m, tea.KeyDown) // → description
	m = typeText(t, m, "代码考古")
	m = press(t, m, tea.KeyDown)  // → model
	m = press(t, m, tea.KeyEnter) // 继承 → the activated model
	m = press(t, m, tea.KeyDown)  // → effort
	m = press(t, m, tea.KeyEnter) // 默认 → low
	m = press(t, m, tea.KeyEnter) // low → medium
	m = press(t, m, tea.KeyDown)  // → save
	m = press(t, m, tea.KeyEnter)

	if m.toast == nil || !strings.Contains(m.toast.text, "已保存") {
		t.Fatalf("save toast missing: %+v", m.toast)
	}
	specs, err := app.Agents()
	if err != nil || len(specs) != 1 {
		t.Fatalf("saved specs = %+v err=%v", specs, err)
	}
	s := specs[0]
	wantModel := m.agentModels[0].Provider + ":" + m.agentModels[0].Model
	if s.Name != "researcher" || s.Description != "代码考古" || s.Model != wantModel || s.Effort != "medium" {
		t.Fatalf("persisted spec = %+v (want model %q)", s, wantModel)
	}
	if view := plain(m.agentsView()); !strings.Contains(view, "researcher") || !strings.Contains(view, "推理 medium") {
		t.Fatalf("list missing the agent:\n%s", view)
	}

	// Edit: reopen the form, the fields carry the saved values (the
	// model field shows the picked choice's label).
	m = press(t, m, tea.KeyEnter)
	if m.agentsStage != agentsStageEdit || m.agentForm.model != wantModel {
		t.Fatalf("edit did not load the form: %+v", m.agentForm)
	}
	if view := plain(m.agentsView()); m.agentsStage != agentsStageEdit || !strings.Contains(view, m.agentModels[0].Label) {
		t.Fatalf("model choice label missing from the form:\n%s", view)
	}
	m = press(t, m, tea.KeyEscape) // back to the list, nothing saved
	if m.agentsStage != agentsStageList {
		t.Fatal("esc did not return to the list")
	}

	// d twice deletes; esc closes; the input never saw a keystroke.
	m = press(t, m, 'd')
	if !m.agentsConfirmDel {
		t.Fatal("first d did not arm the confirm")
	}
	m = press(t, m, 'd')
	if specs, _ = app.Agents(); len(specs) != 0 {
		t.Fatalf("delete left = %+v", specs)
	}
	m = press(t, m, tea.KeyEscape)
	if m.agentsOpen {
		t.Fatal("esc did not close the overlay")
	}
	if m.input.Value() != "" {
		t.Fatalf("input polluted: %q", m.input.Value())
	}
}

// The delegation display (Claude Code's Task style): the row names the
// delegate, live progress rewrites the row in place, the settled row
// carries the report preview.
func TestTaskToolDisplay(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	m := newModel(app, make(chan any, 4))
	m.width, m.height = 80, 24

	args, _ := json.Marshal(map[string]string{"agent": "researcher", "prompt": "找出 emit 的定义位置"})
	call := llm.Block{Kind: llm.BlockToolCall, ID: "t1", Name: subagent.ToolName, Arguments: args}

	m.renderEvent(agent.Event{Type: agent.EvToolStart, Call: &call})
	if view := plain(m.blocks[len(m.blocks)-1].rendered); !strings.Contains(view, "子代理 researcher") {
		t.Fatalf("start row missing the delegate name:\n%s", view)
	}

	m.renderEvent(agent.Event{Type: agent.EvToolProgress, Call: &call, Progress: "researcher · 第 1 步 · read loop.go"})
	if view := plain(m.blocks[len(m.blocks)-1].rendered); !strings.Contains(view, "◐ researcher · 第 1 步") {
		t.Fatalf("progress line missing from the row:\n%s", view)
	}

	res := agent.TextResult("emit 定义在 loop.go:233\n\n(子代理 researcher · 1 轮 · 1 次工具调用 · 模型 m)")
	m.renderEvent(agent.Event{Type: agent.EvToolEnd, Call: &call, Result: &res})
	view := plain(m.blocks[len(m.blocks)-1].rendered)
	if !strings.Contains(view, "子代理 researcher") || !strings.Contains(view, "emit 定义在 loop.go:233") {
		t.Fatalf("settled row missing name or report preview:\n%s", view)
	}
	if strings.Contains(view, "◐") {
		t.Fatalf("stale progress survived the end:\n%s", view)
	}
}
