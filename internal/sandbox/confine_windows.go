//go:build windows

// Windows confinement orchestration (dsh runner.ts + index.ts AclSandbox,
// adapted to an in-process seam): materialize the standing workspace grant
// and the per-run temp grant, build the restricted token, hand it to
// exec.Cmd via SysProcAttr.Token (CreateProcessWithTokenW), and assign the
// child to a kill-on-close Job after Start.
//
// Simplifications vs dsh, by design:
//   - The private temp directory is PER RUN (create/grant/revoke/delete in
//     one call), not per session/workspace pair — one fewer lifecycle, and
//     propagation onto a fresh empty directory is trivially cheap.
//   - The child spawns unsuspended and joins the Job right after Start —
//     a narrow race before assignment (a grandchild spawned in between
//     would escape the Job, NOT the token: confinement is the token's).
package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ConfinedRun owns one confined command's resources: the restricted token,
// the kill-on-close Job (after AfterStart), the per-run temp grant. Close
// releases everything — call it after cmd.Wait returns.
type ConfinedRun struct {
	job     windows.Handle
	onClose func()
}

// Confine prepares cmd to run confined under policy p. It materializes the
// grants, builds the restricted token, rewrites TMP/TEMP to the private
// temp dir, and arms the token on cmd.SysProcAttr (merging, not replacing:
// the caller's HideWindow/process-group flags stand). After cmd.Start,
// call AfterStart to arm the Job; after cmd.Wait, call Close.
//
// Fail-closed: any error revokes what was granted and returns non-nil;
// the caller must NOT start the command.
func Confine(cmd *exec.Cmd, p Policy) (*ConfinedRun, error) {
	if p.Mode != ModeReadOnly && p.Mode != ModeWorkspaceWrite {
		return nil, fmt.Errorf("windows-acl: Confine called with unconfined mode %q", p.Mode)
	}
	workspace := Canonical(p.WorkspaceRoot)
	if st, err := os.Stat(workspace); err != nil || !st.IsDir() {
		return nil, aclErr("Stat", 0, "workspace is not an existing directory: "+workspace)
	}

	var lowSid, worldSid *windows.SID
	var allocated []*windows.SID
	freeAll := func() {
		for _, s := range allocated {
			localFreeSid(s)
		}
	}
	alloc := func(sddl string) (*windows.SID, error) {
		sid, err := parseSid(sddl)
		if err != nil {
			return nil, err
		}
		allocated = append(allocated, sid)
		return sid, nil
	}
	var err error
	if lowSid, err = alloc("S-1-16-4096"); err != nil { // Low mandatory level
		return nil, err
	}
	if worldSid, err = alloc("S-1-1-0"); err != nil { // Everyone
		freeAll()
		return nil, err
	}

	var wsSid, tempSid *windows.SID
	var tempDir string
	cleanupTemp := func() {}
	if p.Mode == ModeWorkspaceWrite {
		if wsSid, err = alloc(workspaceWriteSid(workspace)); err != nil {
			freeAll()
			return nil, err
		}
		// The standing workspace grant: exact-ACE skip makes repeat runs O(1).
		if err := grantWrite(workspace, wsSid, lowSid, worldSid); err != nil {
			freeAll()
			return nil, err
		}
		// Per-run private temp directory, disjoint from the workspace.
		tempParent := Canonical(os.TempDir())
		if under, _ := isPathUnder(tempParent, workspace); under {
			freeAll()
			return nil, aclErr("Confine", 0, fmt.Sprintf("temp root %s must be outside the workspace %s", tempParent, workspace))
		}
		tempDir, err = os.MkdirTemp(tempParent, "scode-sandbox-")
		if err != nil {
			freeAll()
			return nil, aclErr("MkdirTemp", 0, tempParent)
		}
		if tempSid, err = alloc(tempWriteSid(tempDir)); err != nil {
			os.RemoveAll(tempDir) //nolint:errcheck
			freeAll()
			return nil, err
		}
		if err := grantWrite(tempDir, tempSid, lowSid, worldSid); err != nil {
			os.RemoveAll(tempDir) //nolint:errcheck
			freeAll()
			return nil, err
		}
		cleanupTemp = func() {
			// Revoke BEFORE deleting: an inheritable ACE must not outlive
			// its directory (dsh's temp lifecycle).
			if err := revokeWrite(tempDir, tempSid); err != nil {
				fmt.Fprintf(os.Stderr, "windows-acl: cleanup: %v\n", err)
			}
			if err := os.RemoveAll(tempDir); err != nil {
				fmt.Fprintf(os.Stderr, "windows-acl: cleanup: %v\n", err)
			}
		}
		// The child's TMP/TEMP land in the private dir (dsh rewrites them
		// before spawn so tools using temp stay inside the boundary).
		cmd.Env = replaceEnv(cmd.Env, "TMP", tempDir)
		cmd.Env = replaceEnv(cmd.Env, "TEMP", tempDir)
	}

	// Restricted token: keep-alive group (logon + Everyone) plus the write
	// capability SIDs under workspace-write. Default-DACL grant names the
	// temp SID first (a session's pipe objects must not acquire the shared
	// workspace capability), then workspace, then Everyone under read-only.
	current, err := openCurrentProcessToken()
	if err != nil {
		cleanupTemp()
		freeAll()
		return nil, err
	}
	defer windows.CloseHandle(windows.Handle(current)) //nolint:errcheck

	logonSid, logonBuf, err := findLogonSid(current)
	if err != nil {
		cleanupTemp()
		freeAll()
		return nil, err
	}
	_ = logonBuf // keeps the copySID backing memory alive

	writeSids := []*windows.SID{}
	if wsSid != nil {
		writeSids = append(writeSids, wsSid)
	}
	if tempSid != nil {
		writeSids = append(writeSids, tempSid)
	}
	token, err := createRestrictedToken(current, logonSid, worldSid, writeSids, p.Mode)
	if err != nil {
		cleanupTemp()
		freeAll()
		return nil, err
	}
	if err := restrictTokenIntegrity(token, lowSid); err != nil {
		windows.CloseHandle(windows.Handle(token)) //nolint:errcheck
		cleanupTemp()
		freeAll()
		return nil, err
	}
	defaultDaclSid := tempSid
	if defaultDaclSid == nil {
		defaultDaclSid = wsSid
	}
	if defaultDaclSid == nil {
		defaultDaclSid = worldSid
	}
	if err := setTokenDefaultDaclGrant(token, defaultDaclSid); err != nil {
		windows.CloseHandle(windows.Handle(token)) //nolint:errcheck
		cleanupTemp()
		freeAll()
		return nil, err
	}

	// Arm the token, MERGING the caller's SysProcAttr (HideWindow,
	// CREATE_NEW_PROCESS_GROUP).
	attrs := cmd.SysProcAttr
	if attrs == nil {
		attrs = &syscall.SysProcAttr{}
	}
	attrs.Token = syscall.Token(token)
	cmd.SysProcAttr = attrs

	run := &ConfinedRun{}
	run.onClose = func() {
		// Job close kills any still-running descendants (KILL_ON_JOB_CLOSE).
		if run.job != 0 {
			windows.CloseHandle(run.job) //nolint:errcheck
		}
		windows.CloseHandle(windows.Handle(token)) //nolint:errcheck
		cleanupTemp()
		freeAll()
	}
	return run, nil
}

// Close releases the confined run's resources (Job, token, temp grant).
func (r *ConfinedRun) Close() {
	if r.onClose != nil {
		r.onClose()
		r.onClose = nil
	}
}

// AfterStart assigns the started child to a kill-on-close Job: when the
// serve process dies (crash, kill), every confined descendant dies with it.
// Call between cmd.Start and cmd.Wait. Best-effort: failure is reported but
// does not fail the run — confinement comes from the token, the Job is the
// cleanup net.
func (r *ConfinedRun) AfterStart(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return fmt.Errorf("windows-acl: AfterStart before Start")
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return aclErr("CreateJobObject", 0, "")
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job) //nolint:errcheck
		return aclErr("SetInformationJobObject", 0, "KILL_ON_JOB_CLOSE")
	}
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return aclErr("OpenProcess", 0, fmt.Sprintf("pid %d", cmd.Process.Pid))
	}
	defer windows.CloseHandle(proc) //nolint:errcheck
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		windows.CloseHandle(job) //nolint:errcheck
		return aclErr("AssignProcessToJobObject", 0, fmt.Sprintf("pid %d", cmd.Process.Pid))
	}
	r.job = job
	return nil
}

// replaceEnv swaps one key in a KEY=value environment block.
func replaceEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if len(kv) > len(prefix) && strings.EqualFold(kv[:len(prefix)], prefix) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, prefix+value)
}

// Available reports whether this platform's confinement backend can run at
// all (used by diagnostics and tests; Confine stays the fail-closed gate).
func Available() error { return nil }

// activeDenialSignatures is the sole Windows dialect: the restricted token's
// refusals surface as the cmd/pwsh access-denied messages.
func activeDenialSignatures() []string { return DenialSignatures["windows-acl"] }

// RevokeWorkspaceGrant strips the standing workspace-write grant
// materialized for a workspace: the capability SID's ACE, plus the shared Low
// no-write-up label when no other capability grant remains on the directory
// (see revokeWrite). It exists because the grant is server-lifetime by
// design and therefore OUTLIVES the session that created it.
//
// The world FILE_DELETE_CHILD deny ACE is deliberately left in place: it only
// tightens the directory, is harmless to the owner, and removing it would
// re-open the ambient right the grant exists to close.
//
// The grant is inert without a restricted token holding the derived SID, so
// this is an operator/cleanup affordance, not a security requirement.
func RevokeWorkspaceGrant(workspaceRoot string) error {
	ws := Canonical(workspaceRoot)
	if st, err := os.Stat(ws); err != nil || !st.IsDir() {
		return aclErr("RevokeWorkspaceGrant", 0, "workspace is not an existing directory: "+ws)
	}
	sid, err := parseSid(workspaceWriteSid(ws))
	if err != nil {
		return err
	}
	defer localFreeSid(sid)
	return revokeWrite(ws, sid)
}
