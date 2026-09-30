//go:build darwin

// macOS confinement (dsh's sandbox-local darwin chain: seatbelt one rung).
// `sandbox-exec -p <SBPL> -- <cmd>` installs the profile in the wrapper and
// execs the command; the profile denies every file write except the mode's
// writable roots (dsh's seatbeltProfileArgs, built in profiles.go from the
// same root derivation the in-process fs fence uses).
//
// sandbox-exec is deprecated by Apple but still present and functional —
// the only filesystem-boundary primitive available without a kext or
// entitlements. Fail-closed when it is missing.
package sandbox

import (
	"fmt"
	"os"
	"os/exec"
)

// seatbeltExecPath is resolved once; /usr/bin/sandbox-exec is the canonical
// location on every shipping macOS.
func seatbeltExec() (string, error) {
	if path, err := exec.LookPath("sandbox-exec"); err == nil {
		return path, nil
	}
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err == nil {
		return "/usr/bin/sandbox-exec", nil
	}
	return "", fmt.Errorf("SANDBOX_UNAVAILABLE: sandbox-exec was not found; macOS confinement is unavailable")
}

// Confine prepares cmd to run confined under p on macOS.
func Confine(cmd *exec.Cmd, p Policy) (*ConfinedRun, error) {
	if p.Mode != ModeReadOnly && p.Mode != ModeWorkspaceWrite {
		return nil, fmt.Errorf("sandbox-local: Confine called with unconfined mode %q", p.Mode)
	}
	ws := Canonical(p.WorkspaceRoot)
	if st, err := os.Stat(ws); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("sandbox-local: workspace is not an existing directory: %s", ws)
	}
	execPath, err := seatbeltExec()
	if err != nil {
		return nil, err
	}
	inner := append([]string{}, cmd.Args...)
	if len(inner) == 0 {
		inner = []string{cmd.Path}
	}
	wrapper := []string{execPath, "-p", SeatbeltProfile(p), "--"}
	wrapArgv(cmd, append(wrapper, inner...))
	return &ConfinedRun{}, nil
}

// Available reports whether sandbox-exec is present.
func Available() error {
	_, err := seatbeltExec()
	return err
}

// activeDenialSignatures is darwin's sole dialect: Seatbelt refusals surface
// as EPERM.
func activeDenialSignatures() []string { return DenialSignatures["seatbelt"] }
