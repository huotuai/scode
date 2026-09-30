//go:build windows

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// runConfined executes one cmd.exe command line confined under the policy
// and reports its exit code + combined output.
func runConfined(t *testing.T, p Policy, cmdline string) (int, string) {
	t.Helper()
	cmd := exec.Command("cmd.exe", "/c", cmdline)
	cmd.Dir = p.WorkspaceRoot
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
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
		t.Logf("AfterStart (job): %v", err)
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

// workspace-write: the workspace and private temp are writable, everywhere
// else denies writes; reads stay unrestricted.
func TestConfineWorkspaceWriteEnforced(t *testing.T) {
	ws := t.TempDir()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	p := Policy{Mode: ModeWorkspaceWrite, WorkspaceRoot: ws}

	// Write inside the workspace: allowed.
	inside := filepath.Join(ws, "in.txt")
	code, out := runConfined(t, p, `echo hi> `+inside)
	if code != 0 {
		t.Fatalf("workspace write denied: exit %d, out %s", code, out)
	}
	if data, err := os.ReadFile(inside); err != nil || !strings.Contains(string(data), "hi") {
		t.Fatalf("workspace file: %v %q", err, data)
	}

	// Write outside (user profile: ambient-writable for THIS process): denied.
	outsideFile := filepath.Join(home, "scode-confine-outside.txt")
	defer os.Remove(outsideFile) //nolint:errcheck // in case the sandbox leaks
	code, _ = runConfined(t, p, `echo hi> `+outsideFile)
	if code == 0 {
		t.Fatal("outside write was NOT denied")
	}
	if _, err := os.Stat(outsideFile); err == nil {
		t.Fatal("outside file exists — confinement failed")
	}

	// Private temp via the rewritten TMP: allowed, and lands in a private dir.
	code, out = runConfined(t, p, `echo tmp> %TMP%\scode-t.txt && type %TMP%\scode-t.txt`)
	if code != 0 || !strings.Contains(out, "tmp") {
		t.Fatalf("private temp write: exit %d out %q", code, out)
	}

	// Reads outside the boundary are unrestricted (dsh: every mode permits reading).
	code, out = runConfined(t, p, `if exist `+os.Getenv("SystemRoot")+`\System32\kernel32.dll (echo yes) else (echo no)`)
	if code != 0 || !strings.Contains(out, "yes") {
		t.Fatalf("read outside: exit %d out %q", code, out)
	}
}

// read-only: even the workspace denies writes.
func TestConfineReadOnlyEnforced(t *testing.T) {
	ws := t.TempDir()
	p := Policy{Mode: ModeReadOnly, WorkspaceRoot: ws}
	code, _ := runConfined(t, p, `echo hi> `+filepath.Join(ws, "nope.txt"))
	if code == 0 {
		t.Fatal("read-only write was NOT denied")
	}
}

// Grandchildren inherit the restriction (the token travels down the tree).
func TestConfineGrandchildEnforced(t *testing.T) {
	ws := t.TempDir()
	home, _ := os.UserHomeDir()
	p := Policy{Mode: ModeWorkspaceWrite, WorkspaceRoot: ws}
	outsideFile := filepath.Join(home, "scode-confine-grandchild.txt")
	defer os.Remove(outsideFile) //nolint:errcheck
	// cmd → cmd: the grandchild performs the write.
	code, _ := runConfined(t, p, `cmd /c echo hi> `+outsideFile)
	if code == 0 {
		t.Fatal("grandchild write was NOT denied")
	}
	if _, err := os.Stat(outsideFile); err == nil {
		t.Fatal("grandchild escaped the sandbox")
	}
}

// The standing workspace grant is idempotent: a second Confine is an
// exact-ACE skip (no error, no duplicate propagation).
func TestConfineGrantIdempotent(t *testing.T) {
	ws := t.TempDir()
	p := Policy{Mode: ModeWorkspaceWrite, WorkspaceRoot: ws}
	for i := 0; i < 2; i++ {
		code, out := runConfined(t, p, `echo ok> `+filepath.Join(ws, "again.txt"))
		if code != 0 {
			t.Fatalf("run %d: exit %d out %s", i, code, out)
		}
	}
}

func TestDeriveSidDeterministic(t *testing.T) {
	a := workspaceWriteSid(`C:\Proj\A`)
	b := workspaceWriteSid(`C:\Proj\A`)
	if a != b || !strings.HasPrefix(a, "S-1-4-") {
		t.Fatalf("workspace sid: %q vs %q", a, b)
	}
	if tempWriteSid(`C:\T\1`) == workspaceWriteSid(`C:\T\1`) {
		t.Fatal("temp and workspace SIDs must be domain-separated")
	}
	if !strings.HasSuffix(tempWriteSid(`C:\T\1`), "-1") {
		t.Fatal("temp sid carries the third subauthority")
	}
}

// The standing workspace grant is reclaimable: after RevokeWorkspaceGrant the
// capability ACE (and the shared Low label, since nothing else holds one on
// this directory) is gone, while the world FILE_DELETE_CHILD deny ACE — a
// pure tightening — stays. A confined run then re-materializes the grant.
func TestRevokeWorkspaceGrant(t *testing.T) {
	ws := t.TempDir()
	p := Policy{Mode: ModeWorkspaceWrite, WorkspaceRoot: ws}
	// Materialize the grant through a real confined run.
	if code, out := runConfined(t, p, `echo ok> `+filepath.Join(ws, "f.txt")); code != 0 {
		t.Fatalf("confined run: exit %d out %s", code, out)
	}
	sid, err := parseSid(workspaceWriteSid(Canonical(ws)))
	if err != nil {
		t.Fatal(err)
	}
	defer localFreeSid(sid)
	lowSid, err := parseSid("S-1-16-4096")
	if err != nil {
		t.Fatal(err)
	}
	defer localFreeSid(lowSid)
	worldSid, err := parseSid("S-1-1-0")
	if err != nil {
		t.Fatal(err)
	}
	defer localFreeSid(worldSid)

	granted := func() (capAce, label, denyAce bool) {
		acl, labelAcl, _, err := readCurrentSecurity(ws)
		if err != nil {
			t.Fatal(err)
		}
		capAce = acl != nil && hasExactEntry(acl, accessAllowedAceType, inheritAll, grantMask, sid)
		denyAce = acl != nil && hasExactEntry(acl, accessDeniedAceType, inheritContainers, fileDeleteChild, worldSid)
		label = labelAcl != nil && hasExactEntry(labelAcl, mandatoryLabelAce, inheritAll, mandatoryNoWriteUp, lowSid)
		return
	}
	if capAce, label, denyAce := granted(); !capAce || !label || !denyAce {
		t.Fatalf("grant not materialized: cap=%v label=%v deny=%v", capAce, label, denyAce)
	}

	if err := RevokeWorkspaceGrant(ws); err != nil {
		t.Fatalf("RevokeWorkspaceGrant: %v", err)
	}
	if capAce, label, denyAce := granted(); capAce || label {
		t.Errorf("grant survived revoke: cap=%v label=%v", capAce, label)
	} else if !denyAce {
		t.Error("the tightening FILE_DELETE_CHILD deny ACE was removed")
	}

	// Re-materialization still works after a revoke.
	if code, out := runConfined(t, p, `echo again> `+filepath.Join(ws, "g.txt")); code != 0 {
		t.Fatalf("run after revoke: exit %d out %s", code, out)
	}
	if capAce, _, _ := granted(); !capAce {
		t.Error("grant not re-materialized")
	}
	if err := RevokeWorkspaceGrant(ws); err != nil {
		t.Errorf("second revoke: %v", err)
	}
}
