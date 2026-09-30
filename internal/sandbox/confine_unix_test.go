//go:build linux || darwin

// Behavioral confinement tests for the unix backends. They compile-check on
// every host (`GOOS=linux go vet ./...`) and RUN on a Linux/macOS host with
// a usable backend; without one they skip, mirroring Confine's fail-closed
// stance (a missing runner is a deployment fact, not a test failure).
package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runUnix executes one /bin/sh script confined under the policy and reports
// its exit code + combined output.
func runUnix(t *testing.T, p Policy, script string) (int, string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Dir = p.WorkspaceRoot
	cmd.Env = os.Environ()
	run, err := Confine(cmd, p)
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	defer run.Close()
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := run.AfterStart(cmd); err != nil {
		t.Logf("AfterStart: %v", err)
	}
	waitErr := cmd.Wait()
	code := 0
	if exit, ok := waitErr.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if waitErr != nil {
		t.Fatalf("Wait: %v", waitErr)
	}
	return code, out.String()
}

func TestUnixConfine(t *testing.T) {
	if err := Available(); err != nil {
		t.Skipf("no usable backend on this host: %v", err)
	}
	ws := t.TempDir()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory to test the denied side: %v", err)
	}
	outside := filepath.Join(home, fmt.Sprintf("scode-sandbox-denied-%d.txt", os.Getpid()))
	os.Remove(outside) //nolint:errcheck

	// workspace-write: inside is writable.
	inside := filepath.Join(ws, "ok.txt")
	if code, out := runUnix(t, Policy{Mode: ModeWorkspaceWrite, WorkspaceRoot: ws}, "echo hi > '"+inside+"'"); code != 0 {
		t.Fatalf("write inside workspace failed (%d): %s", code, out)
	}
	if _, err := os.Stat(inside); err != nil {
		t.Fatalf("inside file not written: %v", err)
	}

	// workspace-write: outside is refused by the kernel and the file is
	// never created; the output carries a denial signature so bash.go can
	// attach the model-facing marker pair.
	code, out := runUnix(t, Policy{Mode: ModeWorkspaceWrite, WorkspaceRoot: ws}, "echo hi > '"+outside+"'")
	if code == 0 {
		t.Fatalf("write outside the workspace succeeded: %s", out)
	}
	if _, err := os.Stat(outside); err == nil {
		os.Remove(outside) //nolint:errcheck
		t.Fatalf("outside file was created despite the sandbox")
	}
	if !OutputLooksDenied(out) {
		t.Errorf("denial output matches no signature: %q", out)
	}

	// reads stay unrestricted.
	if code, out := runUnix(t, Policy{Mode: ModeWorkspaceWrite, WorkspaceRoot: ws}, "cat /etc/hosts"); code != 0 {
		t.Errorf("read should be unrestricted (%d): %s", code, out)
	}

	// read-only: the workspace itself is not writable.
	if code, _ := runUnix(t, Policy{Mode: ModeReadOnly, WorkspaceRoot: ws}, "echo hi > '"+filepath.Join(ws, "ro.txt")+"'"); code == 0 {
		t.Error("read-only allowed a workspace write")
	}
	if _, err := os.Stat(filepath.Join(ws, "ro.txt")); err == nil {
		t.Error("read-only wrote a file into the workspace")
	}
}

// TestUnixConfineUnavailableIsFailClosed pins the fail-closed contract: with
// no runner the command must NOT start.
func TestUnixConfineUnavailableIsFailClosed(t *testing.T) {
	if err := Available(); err == nil {
		t.Skip("a backend is available; the fail-closed path is unreachable")
	}
	cmd := exec.Command("/bin/sh", "-c", "true")
	if _, err := Confine(cmd, Policy{Mode: ModeReadOnly, WorkspaceRoot: t.TempDir()}); err == nil {
		t.Fatal("Confine must fail closed without a backend")
	}
}
