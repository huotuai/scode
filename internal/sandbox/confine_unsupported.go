//go:build !windows && !linux && !darwin

package sandbox

import (
	"fmt"
	"os/exec"
)

// ConfinedRun is a placeholder on platforms without a confinement backend.
type ConfinedRun struct{}

// Close is a no-op without a backend.
func (r *ConfinedRun) Close() {}

// Confine is fail-closed on unsupported platforms: the command must not run.
func Confine(cmd *exec.Cmd, p Policy) (*ConfinedRun, error) {
	return nil, fmt.Errorf("SANDBOX_UNAVAILABLE: no sandbox runner for this platform yet (mode %s)", p.Mode)
}

// AfterStart is a no-op stub on unsupported platforms.
func (r *ConfinedRun) AfterStart(cmd *exec.Cmd) error { return nil }

// Available reports that no backend exists on this platform.
func Available() error {
	return fmt.Errorf("SANDBOX_UNAVAILABLE: no sandbox runner for this platform")
}

// activeDenialSignatures is empty where no backend exists: nothing can have
// been confined, so no denial dialect applies.
func activeDenialSignatures() []string { return nil }
