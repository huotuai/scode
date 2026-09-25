package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"scode/internal/agent"
	"scode/internal/llm"
)

// BashTool runs shell commands (pi bash.ts schema: command / timeout
// seconds). Shell resolution follows pi's order: SCODE_SHELL override;
// on Windows Git Bash's canonical install paths, then bash on PATH
// (legacy WSL bash gets stdin transport to dodge argv quoting), else a
// descriptive error; on Unix /bin/bash, PATH bash, then sh.
// Divergence from pi: an unset timeout defaults to 120s instead of
// unbounded; process trees die via taskkill /F /T (win) or process-group
// kill (unix).
type BashTool struct{}

func (BashTool) Decl() llm.Tool {
	return llm.Tool{
		Name:        "bash",
		Description: "Run a shell command (bash on all platforms). stdout and stderr are combined; output shows the tail when truncated. Non-zero exits return the output plus the exit code as an error.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"The shell command to run"},"timeout":{"type":"integer","description":"Timeout in seconds (default 120)"}},"required":["command"]}`),
	}
}

const defaultBashTimeout = 120 * time.Second

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
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return agent.ErrorResult("invalid arguments: " + err.Error())
	}
	if strings.TrimSpace(a.Command) == "" {
		return agent.ErrorResult("command is required")
	}
	sh, err := resolveShell()
	if err != nil {
		return agent.ErrorResult(err.Error())
	}
	timeout := defaultBashTimeout
	if a.Timeout > 0 {
		timeout = time.Duration(a.Timeout) * time.Second
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

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		return agent.ErrorResult(fmt.Sprintf("failed to start shell: %v", err))
	}
	var timedOut atomic.Bool
	timer := time.AfterFunc(timeout, func() {
		timedOut.Store(true)
		killProcessTree(cmd)
	})
	waitErr := cmd.Wait()
	timer.Stop()

	output := out.String()
	switch {
	case timedOut.Load():
		return agent.ErrorResult(tailOutput(output) + fmt.Sprintf("\nCommand timed out after %d seconds", int(timeout.Seconds())))
	case waitErr != nil:
		code := 1
		if ee, ok := waitErr.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return agent.ErrorResult(tailOutput(output) + fmt.Sprintf("\nCommand exited with code %d", code))
	default:
		return agent.TextResult(tailOutput(output))
	}
}

// tailOutput tail-truncates shell output (errors live at the end) and
// spills the full output to a temp file when cut.
func tailOutput(s string) string {
	tr := TruncateTail(s)
	if !tr.Truncated {
		return s
	}
	var spill string
	if f, err := os.CreateTemp("", "scode-bash-*.log"); err == nil {
		if _, err := f.WriteString(s); err == nil {
			spill = fmt.Sprintf("\n[Full output saved to %s]", f.Name())
		}
		f.Close()
	}
	return tr.Text + "\n" + tr.Notice + spill
}
