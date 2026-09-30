package tools

// Background bash tasks: a command that outlives its tool call — because
// the caller asked for it (run_in_background) or because it exceeded its
// timeout — is detached into a process-lifetime registry. The model polls
// it with bash_status and stops it with bash_kill; the desktop UI lists and
// manages the same registry over session/tasks.
//
// Ownership rule: the waiter goroutine is the ONLY caller of cmd.Wait and
// the only owner of the confined-run handle. It records the exit result and
// closes the sandbox handle (on Windows the kill-on-close Job, which WOULD
// tear the child down) only after Wait returns and before signalling done —
// so detaching never drops the fence under a still-running child.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"scode/internal/agent"
	"scode/internal/llm"
	"scode/internal/sandbox"
)

// Task states are a closed vocabulary shared with the desktop UI.
const (
	TaskRunning = "running"
	TaskExited  = "exited"
	TaskKilled  = "killed"
)

// Registry bounds: a runaway loop of background commands must not grow the
// process without limit, and retained summaries must not either.
const (
	maxConcurrentBackgroundTasks = 16
	maxRetainedBackgroundTasks   = 32
	// taskWireOutputBytes bounds the output carried per task on the
	// session/tasks wire (the UI polls it): the model-facing bash_status
	// still sees the full 50KB-truncated tail.
	taskWireOutputBytes = 16 * 1024
)

// lockedTailBuffer is tailBuffer with the lock a detached task needs: the
// waiter's copy goroutines write while another tool call (or the UI) reads.
type lockedTailBuffer struct {
	mu sync.Mutex
	b  tailBuffer
}

func newLockedTailBuffer(capacity int) *lockedTailBuffer {
	return &lockedTailBuffer{b: tailBuffer{cap: capacity}}
}

func (l *lockedTailBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedTailBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// backgroundTask owns one detached command.
type backgroundTask struct {
	id        int
	sessionID string
	command   string
	pid       int
	startedAt time.Time
	mode      sandbox.Mode
	confined  bool

	cmd  *exec.Cmd
	out  *lockedTailBuffer
	done chan struct{}

	// confinedRun is closed by the waiter AFTER cmd.Wait returns; the
	// caller must never close it (that would kill a live child on Windows).
	confinedRun *sandbox.ConfinedRun

	mu         sync.Mutex
	finished   bool
	finishedAt time.Time
	killed     bool
	exitCode   int
	waitErr    error
}

// wait is the sole owner of cmd.Wait: it records the terminal state, then
// releases the confined-run handle (Job/token/temp grant), then signals done.
func (t *backgroundTask) wait() {
	err := t.cmd.Wait()
	code := 0
	if err != nil {
		code = 1 // non-ExitError failures report like pi's default
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
	}
	t.mu.Lock()
	t.waitErr = err
	t.exitCode = code
	t.finished = true
	t.finishedAt = time.Now()
	t.mu.Unlock()
	if t.confinedRun != nil {
		t.confinedRun.Close()
	}
	close(t.done)
}

func (t *backgroundTask) exit() (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exitCode, t.waitErr
}

func (t *backgroundTask) isFinished() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.finished
}

// kill signals the process tree; the waiter records the final state.
func (t *backgroundTask) kill() error {
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return fmt.Errorf("background task #%d has already exited", t.id)
	}
	t.killed = true
	t.mu.Unlock()
	killProcessTree(t.cmd)
	return nil
}

// output is the task's captured output, decoded like a foreground command's
// (Windows legacy code pages become UTF-8 at render time, since the buffer
// accumulates raw bytes).
func (t *backgroundTask) output() string {
	return normalizeShellOutput(t.out.String())
}

// taskTail is the model-facing output tail (same truncation/spill
// discipline as a foreground command).
func (t *backgroundTask) taskTail() string {
	return tailOutput(t.output())
}

// denialNotice carries the kernel-refusal marker pair when a confined
// command's output shows a denial — shared by the foreground exit path and
// bash_status so escalation guidance never depends on HOW a command ended.
func (t *backgroundTask) denialNotice() string {
	if !t.confined || !sandbox.OutputLooksDenied(t.output()) {
		return ""
	}
	return "\n" + sandbox.DenialMarker(t.mode) + "\n" + sandbox.EscalationHint("command")
}

// ---------------------------------------------------------------------------
// wire view
// ---------------------------------------------------------------------------

// TaskInfo is the wire/UI view of one background task (session/tasks).
type TaskInfo struct {
	ID         int    `json:"id"`
	SessionID  string `json:"sessionId"`
	Command    string `json:"command"`
	PID        int    `json:"pid"`
	State      string `json:"state"`
	ExitCode   int    `json:"exitCode"`
	StartedAt  int64  `json:"startedAt"` // unix ms
	DurationMs int64  `json:"durationMs"`
	Output     string `json:"output"` // bounded tail (taskWireOutputBytes)
}

func (t *backgroundTask) info() TaskInfo {
	// One lock, one consistent snapshot: a task whose state and duration
	// were read around its completion must not report "exited" with a
	// still-ticking clock. Finished tasks freeze their duration at the
	// moment the waiter recorded the exit.
	t.mu.Lock()
	st := TaskRunning
	exit := 0
	end := time.Now()
	if t.finished {
		st = TaskExited
		if t.killed {
			st = TaskKilled
		}
		exit = t.exitCode
		end = t.finishedAt
	}
	startedAt := t.startedAt
	t.mu.Unlock()
	return TaskInfo{
		ID:         t.id,
		SessionID:  t.sessionID,
		Command:    t.command,
		PID:        t.pid,
		State:      st,
		ExitCode:   exit,
		StartedAt:  startedAt.UnixMilli(),
		DurationMs: end.Sub(startedAt).Milliseconds(),
		Output:     truncateFromEndUTF8(t.output(), taskWireOutputBytes),
	}
}

// ---------------------------------------------------------------------------
// registry
// ---------------------------------------------------------------------------

var bgRegistry = struct {
	mu    sync.Mutex
	tasks map[int]*backgroundTask
	order []int // creation order, for eviction and listing
	next  int
}{tasks: map[int]*backgroundTask{}}

// registerBackgroundTask assigns the id that makes a task visible. A
// refusal (too many live tasks) leaves the caller to kill the command
// rather than leave it unmanaged.
func registerBackgroundTask(t *backgroundTask) error {
	bgRegistry.mu.Lock()
	defer bgRegistry.mu.Unlock()
	running := 0
	for _, x := range bgRegistry.tasks {
		if !x.isFinished() {
			running++
		}
	}
	if running >= maxConcurrentBackgroundTasks {
		return fmt.Errorf("too many background tasks (%d already running); stop one first with bash_kill or the task panel", running)
	}
	if len(bgRegistry.tasks) >= maxRetainedBackgroundTasks {
		evictOldestFinishedLocked()
	}
	bgRegistry.next++
	t.id = bgRegistry.next
	bgRegistry.tasks[t.id] = t
	bgRegistry.order = append(bgRegistry.order, t.id)
	return nil
}

// evictOldestFinishedLocked drops the oldest FINISHED task to keep the
// registry bounded (a finished task's value is its summary). Callers hold
// bgRegistry.mu. The concurrent cap guarantees a finished task exists
// whenever the retained bound is reached.
func evictOldestFinishedLocked() {
	for i, id := range bgRegistry.order {
		t, ok := bgRegistry.tasks[id]
		if !ok || !t.isFinished() {
			continue
		}
		delete(bgRegistry.tasks, id)
		bgRegistry.order = append(bgRegistry.order[:i:i], bgRegistry.order[i+1:]...)
		return
	}
}

// ListBackgroundTasks returns tasks oldest-first, optionally filtered to
// one session ("" = every session).
func ListBackgroundTasks(sessionID string) []TaskInfo {
	bgRegistry.mu.Lock()
	tasks := make([]*backgroundTask, 0, len(bgRegistry.order))
	for _, id := range bgRegistry.order {
		if t, ok := bgRegistry.tasks[id]; ok {
			tasks = append(tasks, t)
		}
	}
	bgRegistry.mu.Unlock()
	out := make([]TaskInfo, 0, len(tasks))
	for _, t := range tasks {
		if sessionID != "" && t.sessionID != sessionID {
			continue
		}
		out = append(out, t.info())
	}
	return out
}

// GetBackgroundTask returns one task's view.
func GetBackgroundTask(id int) (TaskInfo, bool) {
	bgRegistry.mu.Lock()
	t := bgRegistry.tasks[id]
	bgRegistry.mu.Unlock()
	if t == nil {
		return TaskInfo{}, false
	}
	return t.info(), true
}

// KillBackgroundTask kills one task's process tree.
func KillBackgroundTask(id int) error {
	bgRegistry.mu.Lock()
	t := bgRegistry.tasks[id]
	bgRegistry.mu.Unlock()
	if t == nil {
		return fmt.Errorf("no background task #%d", id)
	}
	return t.kill()
}

// KillBackgroundTasksForSession kills every live task of one session, so
// deleting a session never orphans its commands.
func KillBackgroundTasksForSession(sessionID string) {
	bgRegistry.mu.Lock()
	var live []*backgroundTask
	for _, t := range bgRegistry.tasks {
		if t.sessionID == sessionID && !t.isFinished() {
			live = append(live, t)
		}
	}
	bgRegistry.mu.Unlock()
	for _, t := range live {
		_ = t.kill()
	}
}

// KillAllBackgroundTasks kills every live task (app shutdown). Best-effort:
// the Windows sandbox Job is the hard net, the caller's process death the rest.
func KillAllBackgroundTasks() {
	bgRegistry.mu.Lock()
	var live []*backgroundTask
	for _, t := range bgRegistry.tasks {
		if !t.isFinished() {
			live = append(live, t)
		}
	}
	bgRegistry.mu.Unlock()
	for _, t := range live {
		_ = t.kill()
	}
}

// ---------------------------------------------------------------------------
// model-facing rendering (shared by the foreground exit path and bash_status)
// ---------------------------------------------------------------------------

// statusText renders one task for the model.
func (t *backgroundTask) statusText() string {
	info := t.info()
	var b strings.Builder
	fmt.Fprintf(&b, "[background task #%d] %s", info.ID, info.State)
	switch info.State {
	case TaskExited:
		fmt.Fprintf(&b, " (exit code %d)", info.ExitCode)
	case TaskKilled:
		b.WriteString(" (killed)")
	}
	fmt.Fprintf(&b, " · %.1fs", float64(info.DurationMs)/1000)
	if info.State == TaskRunning {
		b.WriteString(" · pid " + strconv.Itoa(info.PID))
	}
	fmt.Fprintf(&b, "\ncommand: %s", info.Command)
	if out := t.taskTail(); out != "" {
		fmt.Fprintf(&b, "\noutput (tail):\n%s", out)
	}
	b.WriteString(t.denialNotice())
	if info.State == TaskRunning {
		b.WriteString("\n(poll again with bash_status; stop it with bash_kill)")
	}
	return b.String()
}

// detachResult registers a task whose caller stopped waiting and reports its
// handle. timeout == 0 marks a deliberate run_in_background. A registry
// refusal kills the command instead of leaking an unmanaged process.
func detachResult(t *backgroundTask, timeout time.Duration) agent.ToolResult {
	if err := registerBackgroundTask(t); err != nil {
		_ = t.kill()
		<-t.done
		return agent.ErrorResult(fmt.Sprintf("%v\n%s", err, t.taskTail()))
	}
	var b strings.Builder
	if timeout > 0 {
		fmt.Fprintf(&b, "[background task #%d] command exceeded %d seconds and is still running in the background.\n", t.id, int(timeout.Seconds()))
	} else {
		fmt.Fprintf(&b, "[background task #%d] command is running in the background (pid %d).\n", t.id, t.pid)
	}
	b.WriteString("Check it with bash_status, stop it with bash_kill.\n")
	fmt.Fprintf(&b, "command: %s", t.command)
	if out := t.taskTail(); out != "" {
		fmt.Fprintf(&b, "\noutput so far:\n%s", out)
	}
	return agent.TextResult(b.String())
}

// foregroundResult renders a command that finished inside its timeout: the
// same exit-code and denial handling as before detaching existed.
func (t *backgroundTask) foregroundResult() agent.ToolResult {
	text := tailOutput(t.output())
	if code, _ := t.exit(); code != 0 {
		return agent.ErrorResult(text + fmt.Sprintf("\nCommand exited with code %d", code) + t.denialNotice())
	}
	return agent.TextResult(text)
}

// commandLine is a bounded one-line rendering of a command for list views.
func commandLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	return ClampLine(s)
}

// ---------------------------------------------------------------------------
// bash_status / bash_kill
// ---------------------------------------------------------------------------

// BashStatusTool reports on detached bash tasks.
type BashStatusTool struct{}

func (BashStatusTool) Decl() llm.Tool {
	return llm.Tool{
		Name:        "bash_status",
		Description: "Check a background bash task: its state, exit code, and output tail. Tasks appear when a bash command hits its timeout or is started with run_in_background. Omit id to list this session's tasks. Query only when you actually need the result; do not poll in a tight loop.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"id":{"type":"number","description":"Background task id from the bash result; omit to list the session's tasks"}}}`),
	}
}

func (BashStatusTool) Execute(tc agent.ToolContext, args json.RawMessage) agent.ToolResult {
	var a struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return agent.ErrorResult("invalid arguments: " + err.Error())
	}
	if a.ID > 0 {
		bgRegistry.mu.Lock()
		t := bgRegistry.tasks[a.ID]
		bgRegistry.mu.Unlock()
		if t == nil {
			return agent.ErrorResult(fmt.Sprintf("no background task #%d (it may have been evicted after finishing; list the session's tasks with bash_status)", a.ID))
		}
		return agent.TextResult(t.statusText())
	}
	tasks := ListBackgroundTasks(tc.Env["SCODE_SESSION_ID"])
	if len(tasks) == 0 {
		return agent.TextResult("no background tasks")
	}
	var b strings.Builder
	b.WriteString("background tasks:")
	for i := range tasks {
		info := &tasks[i]
		fmt.Fprintf(&b, "\n#%d %s", info.ID, info.State)
		switch info.State {
		case TaskExited:
			fmt.Fprintf(&b, " (exit code %d)", info.ExitCode)
		case TaskKilled:
			b.WriteString(" (killed)")
		}
		fmt.Fprintf(&b, " · %.1fs · %s", float64(info.DurationMs)/1000, commandLine(info.Command))
	}
	return agent.TextResult(b.String())
}

func (BashStatusTool) PromptContribution() (string, []string) {
	return "Inspect background bash tasks (bash_status)", []string{
		"After a bash command times out or is started with run_in_background, its result carries a task id: use bash_status to check it and bash_kill to stop it. Poll only when you need the result, not in a tight loop.",
	}
}

// BashKillTool stops a detached bash task.
type BashKillTool struct{}

func (BashKillTool) Decl() llm.Tool {
	return llm.Tool{
		Name:        "bash_kill",
		Description: "Kill a background bash task's process tree (ids come from bash_status).",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"id":{"type":"number","description":"Background task id to kill"}},"required":["id"]}`),
	}
}

func (BashKillTool) Execute(_ agent.ToolContext, args json.RawMessage) agent.ToolResult {
	var a struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return agent.ErrorResult("invalid arguments: " + err.Error())
	}
	if a.ID <= 0 {
		return agent.ErrorResult("id is required")
	}
	if err := KillBackgroundTask(a.ID); err != nil {
		return agent.ErrorResult(err.Error())
	}
	return agent.TextResult(fmt.Sprintf("killed background task #%d", a.ID))
}

func (BashKillTool) PromptContribution() (string, []string) {
	return "Stop a background bash task (bash_kill)", nil
}
