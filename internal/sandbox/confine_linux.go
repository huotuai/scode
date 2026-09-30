//go:build linux

// Linux confinement (dsh's sandbox-local linux chain: bwrap then landlock).
// Both rungs are argv wrappers: bwrap expresses the policy as a mount
// profile (denies by construction — there is simply no writable view of the
// denied paths), and the landlock rung re-execs this binary as a launcher
// that installs an allow-list ruleset on itself and execs the command.
//
// Fail-closed: if neither rung is usable, Confine returns an error and the
// caller must not start the command (never run unconfined).
package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// landlockLauncherArg is the hidden argv[1] that turns this binary into the
// landlock launcher (see landlock_linux.go).
const landlockLauncherArg = "__sandbox_landlock"

type linuxRunnerChoice struct {
	path string
	kind string // "bwrap" | "landlock"
}

var (
	linuxRunnerOnce   sync.Once
	linuxRunnerPicked linuxRunnerChoice
	linuxRunnerErr    error
)

// linuxRunner selects the platform runner chain once: bwrap when it can
// build the profile, else the landlock launcher when the kernel enforces it.
// Both probes are functional (dsh probes once and caches): a version check
// would miss a kernel that has the syscalls but refuses enforcement.
func linuxRunner() (linuxRunnerChoice, error) {
	linuxRunnerOnce.Do(func() {
		if path, err := exec.LookPath("bwrap"); err == nil {
			probe := exec.Command(path, append(BwrapProfileArgs(Policy{Mode: ModeReadOnly, WorkspaceRoot: "/"}), "--", "true")...)
			if probe.Run() == nil {
				linuxRunnerPicked = linuxRunnerChoice{path: path, kind: "bwrap"}
				return
			}
		}
		self, err := selfExe()
		if err != nil {
			linuxRunnerErr = fmt.Errorf("SANDBOX_UNAVAILABLE: cannot resolve the launcher path: %w", err)
			return
		}
		probe := exec.Command(self, landlockLauncherArg, "--probe")
		if out, err := probe.CombinedOutput(); err == nil {
			linuxRunnerPicked = linuxRunnerChoice{path: self, kind: "landlock"}
			return
		} else {
			linuxRunnerErr = fmt.Errorf("SANDBOX_UNAVAILABLE: bwrap is unusable and the landlock launcher is not enforced by this kernel (landlock-run: %s)", firstLine(string(out)))
		}
	})
	if linuxRunnerPicked.path == "" {
		return linuxRunnerChoice{}, linuxRunnerErr
	}
	return linuxRunnerPicked, nil
}

// Confine prepares cmd to run confined under p on Linux.
func Confine(cmd *exec.Cmd, p Policy) (*ConfinedRun, error) {
	if p.Mode != ModeReadOnly && p.Mode != ModeWorkspaceWrite {
		return nil, fmt.Errorf("sandbox-local: Confine called with unconfined mode %q", p.Mode)
	}
	ws := Canonical(p.WorkspaceRoot)
	if st, err := os.Stat(ws); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("sandbox-local: workspace is not an existing directory: %s", ws)
	}
	runner, err := linuxRunner()
	if err != nil {
		return nil, err
	}
	inner := append([]string{}, cmd.Args...)
	if len(inner) == 0 {
		inner = []string{cmd.Path}
	}
	var wrapper []string
	if runner.kind == "bwrap" {
		wrapper = append([]string{runner.path}, BwrapProfileArgs(p)...)
	} else {
		wrapper = append([]string{runner.path, landlockLauncherArg}, LandlockGrantArgs(p)...)
	}
	wrapper = append(wrapper, "--")
	wrapArgv(cmd, append(wrapper, inner...))
	return &ConfinedRun{}, nil
}

// firstLine extracts the first non-empty line of launcher output for the
// availability error.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return "no output"
}

// Available reports whether either Linux rung can confine (bwrap usable, or
// the kernel enforces the landlock launcher).
func Available() error {
	_, err := linuxRunner()
	return err
}

// activeDenialSignatures is the SELECTED Linux rung's dialect, so a bwrap
// failure is never read with Landlock's vocabulary (or vice versa). With no
// usable runner no confined command can have run, so the union is only a
// defensive fallback.
func activeDenialSignatures() []string {
	if c, err := linuxRunner(); err == nil {
		return DenialSignatures[c.kind]
	}
	return append(append([]string{}, DenialSignatures["bwrap"]...), DenialSignatures["landlock"]...)
}
