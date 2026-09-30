package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"scode/internal/agent"
	"scode/internal/llm"
	"scode/internal/sandbox"
)

// BashTool runs shell commands (pi bash.ts schema: command / timeout
// seconds). Shell resolution follows pi's order: SCODE_SHELL override;
// on Windows Git Bash's canonical install paths, then bash on PATH
// (legacy WSL bash gets stdin transport to dodge argv quoting), else a
// descriptive error; on Unix /bin/bash, PATH bash, then sh. Unlike pi
// there IS a default timeout (defaultTimeoutSecs) so a stuck command
// cannot hang the turn forever; the model can still override it up to
// the ~24.8-day hard cap. A command that outlives its call — a timeout,
// or run_in_background — is DETACHED into the background-task registry
// (bash_status / bash_kill) rather than killed. Process trees die via
// taskkill /F /T (win) or process-group kill (unix).
type BashTool struct{}

func (BashTool) Decl() llm.Tool {
	return llm.Tool{
		Name:        "bash",
		Description: "Execute a bash command in the current working directory. Returns stdout and stderr. Output is truncated to last 2000 lines or 50KB (whichever is hit first). If truncated, full output is saved to a temp file. Provide a timeout in seconds (default 120). A command that exceeds its timeout, or is started with run_in_background, keeps running as a background task: inspect it with bash_status and stop it with bash_kill. When the file sandbox confines this session, a command needing a wider file scope may be retried ONCE with sandbox_permissions (the narrowest wider mode that suffices) + justification; the approval prompt asks the user.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"The command to execute"},"timeout":{"type":"number","description":"Timeout in seconds (optional, defaults to 120)"},"run_in_background":{"type":"boolean","description":"Run the command in the background and return a task id immediately"},"sandbox_permissions":{"type":"string","enum":["workspace-write","danger-full-access"],"description":"Sandbox escalation target for this call (requires justification)"},"justification":{"type":"string","description":"One-sentence reason for the sandbox escalation, shown to the user"}},"required":["command"]}`),
	}
}

// maxTimeoutSecs is pi's MAX_TIMEOUT_MS converted (~24.8 days).
const maxTimeoutSecs = 2_147_483_647 / 1000

// defaultTimeoutSecs applies when the model does not pass a timeout,
// so a hung command cannot block the turn indefinitely.
const defaultTimeoutSecs = 120

type shellInfo struct {
	path     string
	args     []string // prefix args: ["-c"], or ["-s"] with stdin transport
	useStdin bool
}

var (
	shellOnce  sync.Once
	resolved   shellInfo
	resolveErr error
)

// resolveConfinedShell picks the shell for a sandboxed execution. MSYS2/
// Cygwin bash cannot run under a Low-integrity restricted token (it dies
// creating \BaseNamedObjects\msys-* directory objects, 0xC0000022), so
// confined commands on Windows run under cmd.exe — POSIX tools stay
// reachable as .exe via PATH; only shell builtins differ.
func resolveConfinedShell() shellInfo {
	if runtime.GOOS == "windows" {
		return shellInfo{path: filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"), args: []string{"/c"}}
	}
	sh, err := resolveShell()
	if err != nil {
		return shellInfo{path: "sh", args: []string{"-c"}}
	}
	return sh
}

func resolveShell() (shellInfo, error) {
	shellOnce.Do(func() {
		if p := os.Getenv("SCODE_SHELL"); p != "" {
			resolved = shellInfo{path: p, args: []string{"-c"}}
			return
		}
		if runtime.GOOS == "windows" {
			for _, c := range []string{
				filepath.Join(os.Getenv("ProgramFiles"), "Git", "bin", "bash.exe"),
				filepath.Join(os.Getenv("ProgramFiles(x86)"), "Git", "bin", "bash.exe"),
			} {
				if c != "" && fileExists(c) {
					resolved = shellInfo{path: c, args: []string{"-c"}}
					return
				}
			}
			if out, err := exec.LookPath("bash"); err == nil {
				// Legacy WSL bash (system32) mangles argv; it reads the
				// script from stdin instead.
				if strings.EqualFold(filepath.Dir(out), filepath.Join(os.Getenv("SystemRoot"), "system32")) {
					resolved = shellInfo{path: out, args: []string{"-s"}, useStdin: true}
				} else {
					resolved = shellInfo{path: out, args: []string{"-c"}}
				}
				return
			}
			resolveErr = fmt.Errorf("no bash found: install Git Bash (https://git-scm.com), or set SCODE_SHELL")
			return
		}
		for _, c := range []string{"/bin/bash", "bash", "sh"} {
			if p, err := exec.LookPath(c); err == nil {
				resolved = shellInfo{path: p, args: []string{"-c"}}
				return
			}
		}
		resolveErr = fmt.Errorf("no shell found on PATH")
	})
	return resolved, resolveErr
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (BashTool) Execute(tc agent.ToolContext, args json.RawMessage) agent.ToolResult {
	var a struct {
		Command            string  `json:"command"`
		Timeout            float64 `json:"timeout"`
		RunInBackground    bool    `json:"run_in_background"`
		SandboxPermissions string  `json:"sandbox_permissions"`
		Justification      string  `json:"justification"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return agent.ErrorResult("invalid arguments: " + err.Error())
	}
	if strings.TrimSpace(a.Command) == "" {
		return agent.ErrorResult("command is required")
	}
	// Sandbox policy for THIS call: the standing mode, or an approved
	// per-call escalation (judged and approved before anything executes).
	policy, escErr := ResolvePolicy(tc, a.SandboxPermissions, a.Justification, a.Command)
	if escErr != nil {
		return *escErr
	}
	// Mode dispatch is exhaustive: only an explicit danger-full-access runs
	// unfenced, and an unresolved mode is refused here too — the fence must
	// never be skipped because a mode string went missing.
	var confinedMode bool
	switch policy.Mode {
	case sandbox.ModeDangerFullAccess:
		confinedMode = false
	case sandbox.ModeReadOnly, sandbox.ModeWorkspaceWrite:
		confinedMode = true
	default:
		return agent.ErrorResult(fmt.Sprintf("sandbox: unresolved mode %q for this call; the command did not run (fail-closed)", policy.Mode))
	}
	var sh shellInfo
	var err error
	if confinedMode {
		sh = resolveConfinedShell()
	} else {
		sh, err = resolveShell()
	}
	if err != nil {
		return agent.ErrorResult(err.Error())
	}
	// Timeout: default applies when omitted; only validity checks here. A
	// command that exceeds it is DETACHED, never killed, so the model can
	// still inspect and stop it.
	if a.Timeout < 0 {
		return agent.ErrorResult("Invalid timeout: must be a finite number of seconds")
	}
	if a.Timeout > maxTimeoutSecs {
		return agent.ErrorResult(fmt.Sprintf("Invalid timeout: maximum is %d seconds", maxTimeoutSecs))
	}

	cmd := exec.Command(sh.path, sh.args...)
	if !sh.useStdin {
		cmd.Args = append(cmd.Args, a.Command)
	} else {
		cmd.Stdin = strings.NewReader(a.Command + "\n")
	}
	cmd.Dir = tc.CWD
	env := os.Environ()
	for k, v := range tc.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	prepareProcess(cmd)

	// Sandbox fence (dsh bash-sandbox semantics): confined modes execute
	// under a WRITE_RESTRICTED token with workspace/temp capability ACLs;
	// without a backend the call fails CLOSED — never runs unrestricted.
	var confined *sandbox.ConfinedRun
	if confinedMode {
		confined, err = sandbox.Confine(cmd, policy)
		if err != nil {
			return agent.ErrorResult(fmt.Sprintf("SANDBOX_UNAVAILABLE: %v — the command did not run", err))
		}
	}

	// Bounded rolling tail: a command emitting gigabytes must not OOM
	// the agent (pi's OutputAccumulator discipline).
	out := newLockedTailBuffer(1 << 20)
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Start(); err != nil {
		if confined != nil {
			confined.Close()
		}
		return agent.ErrorResult(fmt.Sprintf("failed to start shell: %v", err))
	}
	if confined != nil {
		// The Job is the cleanup net, not the boundary: log and run on.
		if err := confined.AfterStart(cmd); err != nil {
			fmt.Fprintf(os.Stderr, "windows-acl: %v\n", err)
		}
	}

	// From here the waiter owns cmd.Wait and the confined handle, so the
	// task stays valid whether this call returns now or on completion.
	task := &backgroundTask{
		sessionID:   tc.Env["SCODE_SESSION_ID"],
		command:     a.Command,
		pid:         cmd.Process.Pid,
		startedAt:   time.Now(),
		mode:        policy.Mode,
		confined:    confined != nil,
		cmd:         cmd,
		out:         out,
		done:        make(chan struct{}),
		confinedRun: confined,
	}
	go task.wait()

	if a.RunInBackground {
		return detachResult(task, 0)
	}
	timeout := time.Duration(a.Timeout * float64(time.Second))
	if timeout <= 0 {
		timeout = defaultTimeoutSecs * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-task.done:
		return task.foregroundResult()
	case <-timer.C:
		// Timeout: hand the command to the registry instead of killing it.
		// If it finished in the very instant the timer fired, its own result
		// is the truthful one — there is no live task to manage.
		if task.isFinished() {
			return task.foregroundResult()
		}
		return detachResult(task, timeout)
	case <-ctxDone(tc.Ctx):
		// Cancellation (Ctrl-C / abort) still kills the tree — a runaway
		// foreground command must not outlive its turn.
		_ = task.kill()
		<-task.done
		return agent.ErrorResult(task.taskTail() + "\nCommand aborted")
	}
}

// ctxDone returns the context's Done channel, or nil when there is no
// context (a nil channel blocks forever in a select — exactly what an
// absent run context means: nothing can abort this call).
func ctxDone(ctx context.Context) <-chan struct{} {
	if ctx == nil {
		return nil
	}
	return ctx.Done()
}

// tailBuffer keeps only the last cap bytes written to it.
type tailBuffer struct {
	b   []byte
	cap int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.cap {
		t.b = t.b[len(t.b)-t.cap:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }

// tailOutput tail-truncates shell output (errors live at the end) and
// spills the full output to a temp file when cut (pi's notice shape).
func tailOutput(s string) string {
	tr := TruncateTail(s)
	if !tr.Truncated {
		return s
	}
	spill := ""
	if f, err := os.CreateTemp("", "scode-bash-*.log"); err == nil {
		if _, err := f.WriteString(s); err == nil {
			spill = fmt.Sprintf(" Full output: %s", f.Name())
		}
		f.Close()
	}
	totalLines := strings.Count(s, "\n") + 1
	keptLines := strings.Count(strings.TrimRight(tr.Text, "\n"), "\n") + 1
	start := totalLines - keptLines + 1
	if start < 1 {
		start = 1
	}
	return tr.Text + fmt.Sprintf("\n\n[Showing lines %d-%d of %d (50KB limit).%s]", start, totalLines, totalLines, spill)
}

// PromptContribution is pi's bashToolSystemPromptContribution (with
// scode's SCODE_* env names in place of PI_*).
func (BashTool) PromptContribution() (string, []string) {
	return "Execute bash commands (ls, grep, find, etc.)", []string{"You can inspect SCODE_* environment variables for current model and session details."}
}
