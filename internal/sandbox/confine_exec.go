//go:build linux || darwin

// Argv-wrap confinement scaffolding shared by the unix backends (bwrap and
// the landlock launcher on linux, sandbox-exec on darwin): the wrapper
// process IS the boundary — it installs the profile and execs the command,
// so there are no kernel handles to own or revoke. dsh's runner seam
// (`ConfinedArgv`) has the same shape.
package sandbox

import (
	"os"
	"os/exec"
)

// ConfinedRun is a handle for one argv-wrapped run. The unix backends keep
// no resources: whatever the wrapper installed dies with the wrapper and
// the confined tree.
type ConfinedRun struct{}

// Close is a no-op for argv-wrap backends.
func (r *ConfinedRun) Close() {}

// AfterStart is a no-op for argv-wrap backends (no job object: bwrap's
// --die-with-parent and the launcher's exec are the lifetime story).
func (r *ConfinedRun) AfterStart(cmd *exec.Cmd) error { return nil }

// wrapArgv replaces the command's invocation with a wrapper argv, keeping
// the caller's SysProcAttr (process-group flags) untouched.
func wrapArgv(cmd *exec.Cmd, argv []string) {
	cmd.Path = argv[0]
	cmd.Args = argv
}

// selfExe resolves this binary's path for the self-re-exec launcher rung.
func selfExe() (string, error) {
	return os.Executable()
}
