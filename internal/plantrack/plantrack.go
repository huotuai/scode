// Package plantrack implements plan progress: the agent's working plan
// as an observable, persisted checklist. The update_plan tool replaces
// the whole list on every call (Codex's update_plan contract); the
// Tracker holds the current state and fires OnChange once per actual
// update, which the host wires to session persistence (a plan entry —
// log-only, whole-value replace, last wins — so resume/fork replay the
// plan like the mode/sandbox entries). The desktop polls session/plan
// to render progress. The checklist is per-task: the tool description
// directs the model to replace it wholesale when a new user message
// begins a distinct task, so a finished task's steps never linger on
// the user's progress display.
package plantrack

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"scode/internal/agent"
	"scode/internal/llm"
	"scode/internal/session"
)

// Statuses for a plan step (session.PlanItem.Status).
const (
	Pending    = "pending"
	InProgress = "in_progress"
	Completed  = "completed"
)

// Tracker owns the plan state. Updates fire OnChange once per accepted
// update; the host wires OnChange to a persisted plan entry.
type Tracker struct {
	mu       sync.Mutex
	plan     session.PlanEntry
	set      bool // true once a plan exists (Restore or the first update)
	OnChange func(plan session.PlanEntry)
}

// Set replaces the plan, firing OnChange. The entry is validated by the
// tool before it reaches here.
func (t *Tracker) Set(plan session.PlanEntry) {
	t.mu.Lock()
	t.plan = plan
	t.set = true
	onChange := t.OnChange
	t.mu.Unlock()
	if onChange != nil {
		onChange(plan)
	}
}

// Restore sets the state without firing OnChange (load-time replay:
// the state is already durable in the session log).
func (t *Tracker) Restore(plan session.PlanEntry) {
	t.mu.Lock()
	t.plan = plan
	t.set = true
	t.mu.Unlock()
}

// Reset clears the plan without firing OnChange (cli /new: the fresh
// session starts with no checklist and nothing to persist).
func (t *Tracker) Reset() {
	t.mu.Lock()
	t.plan = session.PlanEntry{}
	t.set = false
	t.mu.Unlock()
}

// Get returns the current plan; ok is false when no plan was ever set
// (the desktop renders its empty state then).
func (t *Tracker) Get() (plan session.PlanEntry, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.plan, t.set
}

// ---------------------------------------------------------------------------
// update_plan (Codex's tool contract: whole-list replace, progress summary)
// ---------------------------------------------------------------------------

// UpdateToolName is the permanently registered tool name.
const UpdateToolName = "update_plan"

const updateDescription = `Update the working plan and report progress. Use it whenever the task has multiple steps: create the plan before starting the work (mark the first step in_progress), and update it as steps complete so the user can follow the progress. Do not use it for trivial single-step tasks. The checklist is per-TASK, not per-session: when a new user message begins a distinct multi-step task, start a FRESH plan that replaces the previous task's checklist wholesale — never leave an old task's completed steps on screen; when the task finishes, close the plan out with a final call marking every step completed; when the user redirects mid-task, rewrite the plan to the new direction instead of annotating a stale one. This tool is the ONLY channel for progress — a plan narrated in chat text never reaches the user's checklist. The call replaces the WHOLE plan — resend every step with its current status on each update. Each step's status is pending, in_progress, or completed; at most one step may be in_progress at a time. The plan is persisted with the session and shown to the user as a progress checklist.`

// UpdateTool maintains the session's plan checklist.
type UpdateTool struct {
	trk *Tracker
}

// NewUpdateTool wires the tool to the tracker.
func NewUpdateTool(trk *Tracker) *UpdateTool {
	return &UpdateTool{trk: trk}
}

func (t *UpdateTool) Decl() llm.Tool {
	return llm.Tool{
		Name:        UpdateToolName,
		Description: updateDescription,
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"explanation":{"type":"string","description":"Optional one-line note about what changed in this update."},` +
			`"plan":{"type":"array","description":"The complete plan, replacing the previous one; a new task replaces the old task's list entirely.","items":{"type":"object","properties":{` +
			`"step":{"type":"string","description":"Short description of the step."},` +
			`"status":{"type":"string","enum":["pending","in_progress","completed"],"description":"Step status; at most one step may be in_progress."}` +
			`},"required":["step","status"],"additionalProperties":false}}` +
			`},"required":["plan"],"additionalProperties":false}`),
	}
}

// Execute validates the whole-list update and commits it to the
// tracker (which persists it via OnChange). Validation failures come
// back as error results so the model can fix and resend.
func (t *UpdateTool) Execute(tc agent.ToolContext, args json.RawMessage) agent.ToolResult {
	var a struct {
		Explanation string             `json:"explanation"`
		Plan        []session.PlanItem `json:"plan"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return agent.ErrorResult("invalid arguments: " + err.Error())
	}
	if len(a.Plan) == 0 {
		return agent.ErrorResult(UpdateToolName + " requires a non-empty plan array")
	}
	inProgress := 0
	for i, item := range a.Plan {
		a.Plan[i].Step = strings.TrimSpace(item.Step)
		if a.Plan[i].Step == "" {
			return agent.ErrorResult(fmt.Sprintf("plan item %d has an empty step", i+1))
		}
		switch item.Status {
		case Pending, Completed:
		case InProgress:
			inProgress++
		case "":
			a.Plan[i].Status = Pending
		default:
			return agent.ErrorResult(fmt.Sprintf("plan item %d has invalid status %q (pending|in_progress|completed)", i+1, item.Status))
		}
	}
	if inProgress > 1 {
		return agent.ErrorResult("at most one plan step may be in_progress")
	}
	entry := session.PlanEntry{Explanation: strings.TrimSpace(a.Explanation), Items: a.Plan}
	t.trk.Set(entry)
	return agent.TextResult(render(entry))
}

// render is the tool result: a one-line summary plus the checklist, so
// the model sees the committed state it just sent.
func render(plan session.PlanEntry) string {
	done := 0
	var b strings.Builder
	for _, item := range plan.Items {
		if item.Status == Completed {
			done++
		}
	}
	fmt.Fprintf(&b, "Plan updated: %d/%d completed.", done, len(plan.Items))
	if plan.Explanation != "" {
		b.WriteString("\n" + plan.Explanation)
	}
	for _, item := range plan.Items {
		mark := "[ ]"
		switch item.Status {
		case InProgress:
			mark = "[>]"
		case Completed:
			mark = "[x]"
		}
		fmt.Fprintf(&b, "\n%s %s", mark, item.Step)
	}
	return b.String()
}
