//go:build !linux

package sandbox

// RunLauncherCommand reports whether this invocation is a sandbox launcher
// self-re-exec. Only Linux ships a self-re-exec launcher (the landlock
// rung); the other backends wrap an external binary instead.
func RunLauncherCommand() bool { return false }
