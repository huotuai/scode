package plantrack

import (
	"encoding/json"
	"strings"
	"testing"

	"scode/internal/agent"
	"scode/internal/session"
)

func exec(t *testing.T, tool *UpdateTool, args string) agent.ToolResult {
	t.Helper()
	return tool.Execute(agent.ToolContext{}, json.RawMessage(args))
}

func resultText(r agent.ToolResult) string {
	if len(r.Content) == 0 {
		return ""
	}
	return r.Content[0].Text
}

// The description carries the task-boundary contract — the model's
// only guidance (the system prompt never mentions update_plan). Pin
// the key phrases so a future edit cannot silently drop them.
func TestUpdateDescriptionSemantics(t *testing.T) {
	d := NewUpdateTool(&Tracker{}).Decl().Description
	for _, want := range []string{
		"per-TASK, not per-session",              // a new task replaces the old checklist
		"replaces the previous task's checklist", // stale completed steps never linger
		"ONLY channel for progress",              // prose plans never reach the checklist
		"replaces the WHOLE plan",                // every call resends every step
	} {
		if !strings.Contains(d, want) {
			t.Fatalf("update_plan description lost %q:\n%s", want, d)
		}
	}
	var schema any
	if err := json.Unmarshal(NewUpdateTool(&Tracker{}).Decl().Parameters, &schema); err != nil {
		t.Fatalf("update_plan schema is not valid JSON: %v", err)
	}
}

func TestUpdatePlanSetsStateAndFiresOnChange(t *testing.T) {
	trk := &Tracker{}
	var persisted []session.PlanEntry
	trk.OnChange = func(plan session.PlanEntry) { persisted = append(persisted, plan) }

	r := exec(t, NewUpdateTool(trk), `{"plan":[
		{"step":"调研","status":"completed"},
		{"step":"实现","status":"in_progress"},
		{"step":"验证","status":"pending"}
	]}`)
	if r.IsError {
		t.Fatalf("unexpected error: %s", resultText(r))
	}
	text := resultText(r)
	if !strings.Contains(text, "Plan updated: 1/3 completed.") ||
		!strings.Contains(text, "[x] 调研") ||
		!strings.Contains(text, "[>] 实现") ||
		!strings.Contains(text, "[ ] 验证") {
		t.Fatalf("result = %q", text)
	}
	plan, ok := trk.Get()
	if !ok || len(plan.Items) != 3 || plan.Items[1].Status != InProgress {
		t.Fatalf("state = %+v, ok=%v", plan, ok)
	}
	if len(persisted) != 1 || persisted[0].Items[0].Step != "调研" {
		t.Fatalf("OnChange fired %d times with %+v", len(persisted), persisted)
	}
}

func TestUpdatePlanRejectsBadInput(t *testing.T) {
	trk := &Tracker{}
	tool := NewUpdateTool(trk)
	cases := []string{
		`{"plan":[]}`,
		`{"plan":[{"step":"","status":"pending"}]}`,
		`{"plan":[{"step":"a","status":"done"}]}`,
		`{"plan":[{"step":"a","status":"in_progress"},{"step":"b","status":"in_progress"}]}`,
	}
	for _, c := range cases {
		if r := exec(t, tool, c); !r.IsError {
			t.Fatalf("%s: want error, got %q", c, resultText(r))
		}
	}
	if _, ok := trk.Get(); ok {
		t.Fatal("rejected updates must not set the plan")
	}
}

func TestUpdatePlanDefaultsStatusAndTrims(t *testing.T) {
	trk := &Tracker{}
	r := exec(t, NewUpdateTool(trk), `{"explanation":" 开始 ","plan":[{"step":" 只做一步 "}]}`)
	if r.IsError {
		t.Fatalf("unexpected error: %s", resultText(r))
	}
	plan, ok := trk.Get()
	if !ok || plan.Items[0].Status != Pending || plan.Items[0].Step != "只做一步" || plan.Explanation != "开始" {
		t.Fatalf("state = %+v, ok=%v", plan, ok)
	}
}

func TestRestoreDoesNotFireOnChange(t *testing.T) {
	trk := &Tracker{}
	trk.Restore(session.PlanEntry{Items: []session.PlanItem{{Step: "s", Status: Completed}}})
	fired := false
	trk.OnChange = func(session.PlanEntry) { fired = true }
	trk.Restore(session.PlanEntry{Items: []session.PlanItem{{Step: "s2", Status: Pending}}})
	if fired {
		t.Fatal("Restore must not fire OnChange (the state is already durable)")
	}
	plan, ok := trk.Get()
	if !ok || plan.Items[0].Step != "s2" {
		t.Fatalf("state = %+v, ok=%v", plan, ok)
	}
}
