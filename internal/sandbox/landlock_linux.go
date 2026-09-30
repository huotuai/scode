//go:build linux

// The landlock rung's launcher (dsh's landlock-run C entry, re-implemented
// over Go's raw syscalls): this binary re-execs itself as
//
//	scode __sandbox_landlock [--ro <path>]... [--rw <path>]... -- <argv>...
//	scode __sandbox_landlock --probe
//
// installs an allow-list ruleset on ITSELF, then execs the wrapped command.
// Landlock is inherited across execve, so the command and every descendant
// run confined while the serve process stays unrestricted — the only way to
// confine a process with Landlock is for the process to restrict itself
// before exec.
//
// Fail-closed: if the kernel does not enforce Landlock, exit non-zero
// WITHOUT exec'ing. Fatal errors print `landlock-run: <detail>` and exit
// 125 (dsh's launcher contract, so command failures stay distinguishable).
package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Landlock UAPI (stable kernel contract; the Go runtime library has no
// wrappers, so the raw syscalls and structs live here). Numbers are
// identical on every architecture's unified syscall table.
const (
	llSysCreateRuleset = 444
	llSysAddRule       = 445
	llSysRestrictSelf  = 446

	llCreateRulesetVersion = 1 << 0 // landlock_create_ruleset flags: report ABI
	llRulePathBeneath      = 1      // landlock_rule_type
	llExitLauncherFailure  = 125    // dsh's runner-failure exit
	llMaxABI               = 5
)

// Filesystem access bits, grouped by the ABI that introduced them.
const (
	llFsExecute    = uint64(1) << 0
	llFsWriteFile  = uint64(1) << 1
	llFsReadFile   = uint64(1) << 2
	llFsReadDir    = uint64(1) << 3
	llFsRemoveDir  = uint64(1) << 4
	llFsRemoveFile = uint64(1) << 5
	llFsMakeChar   = uint64(1) << 6
	llFsMakeDir    = uint64(1) << 7
	llFsMakeReg    = uint64(1) << 8
	llFsMakeSock   = uint64(1) << 9
	llFsMakeFifo   = uint64(1) << 10
	llFsMakeBlock  = uint64(1) << 11
	llFsMakeSym    = uint64(1) << 12
	llFsRefer      = uint64(1) << 13 // ABI 2
	llFsTruncate   = uint64(1) << 14 // ABI 3
	llFsIoctlDev   = uint64(1) << 15 // ABI 5

	llABI1Mask  = llFsRefer - 1 // bits 0..12: every ABI-1 access
	llReadSide  = llFsExecute | llFsReadFile | llFsReadDir
	llFileCombo = llFsExecute | llFsWriteFile | llFsReadFile | llFsTruncate | llFsIoctlDev
)

// landlockPathBeneathAttr mirrors the kernel's PACKED struct. Go lays a
// uint64 then an int32 at offsets 0 and 8 — the same offsets the packed
// kernel layout uses, and the kernel reads exactly 12 bytes.
type landlockPathBeneathAttr struct {
	allowedAccess uint64
	parentFd      int32
}

// RunLauncherCommand turns this process into the landlock launcher when
// argv says so; returns false otherwise. Never returns true (the launcher
// either execs or exits).
func RunLauncherCommand() bool {
	if len(os.Args) < 2 || os.Args[1] != landlockLauncherArg {
		return false
	}
	os.Exit(landlockMain(os.Args[2:]))
	return true
}

// llFail prints one fatal `landlock-run: ...` line and returns the exit code.
func llFail(detail string) int {
	fmt.Fprintf(os.Stderr, "landlock-run: %s\n", detail)
	return llExitLauncherFailure
}

// llSys is one raw Landlock syscall (3-argument form).
func llSys(trap, a1, a2, a3 uintptr) (int, error) {
	r1, _, errno := unix.Syscall(trap, a1, a2, a3)
	if errno != 0 {
		return -1, errno
	}
	return int(r1), nil
}

// llVersion queries the running kernel's Landlock ABI (negative on error).
func llVersion() (int64, error) {
	r1, _, errno := unix.Syscall(llSysCreateRuleset, 0, 0, llCreateRulesetVersion)
	if errno != 0 {
		return 0, errno
	}
	return int64(r1), nil
}

// llMaskForABI is the set of accesses the running ABI can actually govern.
func llMaskForABI(abi int64) uint64 {
	mask := llABI1Mask
	if abi >= 2 {
		mask |= llFsRefer
	}
	if abi >= 3 {
		mask |= llFsTruncate
	}
	if abi >= 5 {
		mask |= llFsIoctlDev
	}
	return mask
}

// llAddRule adds one path-beneath allow rule. Non-directory grants keep only
// the file-compatible bits (the kernel rejects dir-only accesses on a file —
// this is how `--rw /dev/null` works).
func llAddRule(rulesetFd int, path string, access uint64) error {
	pathFd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("cannot open rule path: %s: %w", path, err)
	}
	defer unix.Close(pathFd) //nolint:errcheck
	var st unix.Stat_t
	if err := unix.Fstat(pathFd, &st); err == nil && st.Mode&unix.S_IFMT != unix.S_IFDIR {
		access &= llFileCombo
	}
	attr := landlockPathBeneathAttr{allowedAccess: access, parentFd: int32(pathFd)}
	if _, err := llSys(llSysAddRule, uintptr(rulesetFd), llRulePathBeneath, uintptr(unsafe.Pointer(&attr))); err != nil {
		return fmt.Errorf("landlock ruleset error: %w", err)
	}
	return nil
}

// llRestrictSelf negotiates the ABI, builds the ruleset, sets no_new_privs
// (mandatory for an unprivileged restrict, and it neutralizes setuid), and
// installs it. partial reports enforcement narrowed to an older ABI — still
// confined for everything the kernel supports, so it is reported, not refused.
func llRestrictSelf(ro, rw []string) (partial bool, err error) {
	abi, err := llVersion()
	if err != nil {
		return false, fmt.Errorf("landlock is not enforced by this kernel (ABI unsupported or disabled): %w", err)
	}
	partial = abi < llMaxABI
	handled := llMaskForABI(abi)
	if abi > llMaxABI {
		handled = llMaskForABI(llMaxABI)
	}
	rulesetFd, err := llSys(llSysCreateRuleset, uintptr(unsafe.Pointer(&handled)), unsafe.Sizeof(handled), 0)
	if err != nil {
		return false, fmt.Errorf("landlock ruleset error: %w", err)
	}
	defer unix.Close(rulesetFd) //nolint:errcheck
	for _, path := range ro {
		if err := llAddRule(rulesetFd, path, llReadSide&handled); err != nil {
			return false, err
		}
	}
	for _, path := range rw {
		if err := llAddRule(rulesetFd, path, handled); err != nil {
			return false, err
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return false, fmt.Errorf("landlock ruleset error: %w", err)
	}
	if _, err := llSys(llSysRestrictSelf, uintptr(rulesetFd), 0, 0); err != nil {
		return false, fmt.Errorf("landlock ruleset error: %w", err)
	}
	return partial, nil
}

// landlockMain parses the launcher CLI, restricts this process, and execs.
func landlockMain(args []string) int {
	var ro, rw []string
	probe := false
	index := 0
	for index < len(args) {
		arg := args[index]
		switch {
		case arg == "--probe":
			probe = true
			index++
		case arg == "--ro" || arg == "--rw":
			if index+1 >= len(args) {
				return llFail("usage error: " + arg + " requires a path")
			}
			if arg == "--ro" {
				ro = append(ro, args[index+1])
			} else {
				rw = append(rw, args[index+1])
			}
			index += 2
		case arg == "--":
			index++
			goto exec
		default:
			return llFail("usage error: unknown argument: " + arg)
		}
	}
exec:
	command := args[index:]
	if !probe && len(command) == 0 {
		return llFail("usage error: missing `-- <argv>...` command")
	}
	if probe {
		// The functional probe: actually restrict THIS short-lived process
		// (a version check would miss a kernel that has the syscalls but
		// refuses enforcement).
		partial, err := llRestrictSelf([]string{"/"}, nil)
		if err != nil {
			return llFail(err.Error())
		}
		if partial {
			fmt.Println("landlock: partially enforced (older ABI)")
		} else {
			fmt.Println("landlock: fully enforced")
		}
		return 0
	}
	partial, err := llRestrictSelf(ro, rw)
	if err != nil {
		return llFail(err.Error())
	}
	if partial {
		fmt.Fprintln(os.Stderr, "landlock-run: partial enforcement (older Landlock ABI)")
	}
	resolved, err := exec.LookPath(command[0])
	if err != nil {
		return llFail("exec failed: " + err.Error())
	}
	if err := unix.Exec(resolved, command, os.Environ()); err != nil {
		return llFail("exec failed: " + err.Error())
	}
	return 0 // unreachable
}
