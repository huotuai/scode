// Package sandbox ports dsh's sandbox-policy + fs-sandbox semantics: a
// per-session file-effect mode (read-only / workspace-write /
// danger-full-access), the canonical writable-root derivation shared by
// every enforcement dialect, and the containment fence for the file tools.
//
// The fence is a policy check in TRUSTED code over a MODEL-CONTROLLED path,
// not a kernel boundary — kernel-grade confinement of bash belongs to the
// runner backends (acl_windows.go, landlock, seatbelt). Reads are never
// restricted, mirroring dsh: every mode permits reading.
package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// Mode is the file-effect policy vocabulary (dsh's SandboxMode).
type Mode string

const (
	// ModeReadOnly denies every mutation.
	ModeReadOnly Mode = "read-only"
	// ModeWorkspaceWrite allows mutations under the workspace root plus the
	// platform temp area.
	ModeWorkspaceWrite Mode = "workspace-write"
	// ModeDangerFullAccess does not restrict mutations.
	ModeDangerFullAccess Mode = "danger-full-access"
)

// Modes is the closed vocabulary, in display order.
var Modes = []Mode{ModeReadOnly, ModeWorkspaceWrite, ModeDangerFullAccess}

// ParseMode validates a mode string (config load, RPC input).
func ParseMode(s string) (Mode, error) {
	if m := Mode(s); m.Valid() {
		return m, nil
	}
	return "", fmt.Errorf("invalid sandbox mode %q (want read-only | workspace-write | danger-full-access)", s)
}

// Valid reports whether the mode is one of the closed vocabulary. An empty
// or misspelled mode is a wiring bug, NOT a synonym for unrestricted: every
// consumer fails closed on it (see ErrNoMode).
func (m Mode) Valid() bool {
	switch m {
	case ModeReadOnly, ModeWorkspaceWrite, ModeDangerFullAccess:
		return true
	}
	return false
}

// ErrNoMode guards the fail-closed contract: a policy whose mode was never
// resolved must not be read as "unrestricted". Callers surface it verbatim
// (it carries no escalation hint — there is nothing to escalate along).
var ErrNoMode = errors.New("sandbox: no mode is resolved for this call; refusing to operate unfenced")

// Policy is the fully resolved per-call policy (dsh's
// SandboxExecutionPolicy): the effective mode plus the workspace boundary.
type Policy struct {
	Mode          Mode
	WorkspaceRoot string
}

// DenialError marks a sandbox policy refusal; the tool layer maps it to the
// model-facing denial marker.
type DenialError struct {
	Mode Mode
	Path string
}

func (e *DenialError) Error() string {
	return fmt.Sprintf("cannot write %q: file access denied under %s mode", e.Path, e.Mode)
}

// DenialMarker is the model-facing denial vocabulary, verbatim from dsh's
// escalation.ts so the model recognizes a policy denial identically whether
// the fs fence or a kernel runner refused.
func DenialMarker(mode Mode) string {
	return fmt.Sprintf("[sandbox: file access denied under %s mode]", mode)
}

// EscalationHint rides a denial when escalation fields are advertised
// (verbatim from dsh's escalationHintMarker, subject = the family noun).
func EscalationHint(subject string) string {
	return fmt.Sprintf("[sandbox: escalation available — retry this exact %s once with sandbox_permissions (the narrowest wider mode that suffices) + justification; the approval prompt asks the user]", subject)
}

// Canonical resolves a granted root or target to the spelling the
// enforcement layer compares: symlinks resolved, because containment must
// match filesystem identity (darwin's /tmp IS /private/tmp, and a Windows
// junction is invisible to a lexical check).
//
// The resolution follows dsh's fsio.resolve: realpath the DEEPEST EXISTING
// ancestor and re-append the missing suffix. Resolving only the whole path
// is not enough — a missing target ("write a new file") would fall back to a
// lexical spelling, and any symlinked/junctioned ancestor would then read as
// contained when it is not.
//
// An unresolvable EXISTING component (permission or I/O fault) is reported
// as an error to the caller; Canonical itself keeps the conservative
// lexical spelling so non-security callers (root derivation, prompt text)
// still produce a usable path.
func Canonical(path string) string {
	resolved, err := canonical(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return resolved
}

// canonical is Canonical with the resolution failure surfaced, for callers
// that must fail closed instead of guessing (CheckWrite).
func canonical(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var missing []string
	for cur := abs; ; {
		resolved, err := evalExistingPath(cur)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !unresolvable(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// No component exists (a fully virtual path): the cleaned
			// absolute spelling is the honest answer.
			return filepath.Clean(abs), nil
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
}

// unresolvable reports whether a resolution failure means "this component
// does not exist" (walk up and retry) rather than a real fault. ENOTDIR
// counts: a path whose ancestor is a regular file cannot exist, and the
// mutation must produce its natural error, not a sandbox denial.
func unresolvable(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// WritableRoots derives the mode's meaning as a canonical, deduplicated
// allow-list (dsh's writableRoots): workspace-write = the workspace root,
// the per-user temp dir, and (on unix) the host /tmp — darwin's /tmp is
// /private/tmp once canonicalized. read-only allows nothing.
func (p Policy) WritableRoots() []string {
	if p.Mode != ModeWorkspaceWrite {
		return nil
	}
	candidates := []string{p.WorkspaceRoot, os.TempDir()}
	if runtime.GOOS != "windows" {
		candidates = append(candidates, "/tmp")
	}
	seen := map[string]bool{}
	var roots []string
	for _, r := range candidates {
		c := Canonical(r)
		if !seen[c] {
			seen[c] = true
			roots = append(roots, c)
		}
	}
	return roots
}

// CheckWrite enforces the policy against a mutation target and returns the
// EXACT canonical path the mutation must use, so the checked identity is the
// mutated one (no check-here-write-there TOCTOU — dsh's checkedTarget).
// danger-full-access returns the input unfenced; read-only denies;
// workspace-write canonicalizes NOW and requires containment. An unresolved
// mode denies too: absence of a policy is never permission.
func (p Policy) CheckWrite(path string) (string, error) {
	switch p.Mode {
	case ModeDangerFullAccess:
		return path, nil
	case ModeReadOnly:
		return "", &DenialError{Mode: p.Mode, Path: path}
	case ModeWorkspaceWrite:
		// containment check below
	default:
		return "", ErrNoMode
	}
	fresh, cerr := canonical(path)
	if cerr != nil {
		return "", cerr
	}
	for _, root := range p.WritableRoots() {
		under, err := isPathUnder(fresh, root)
		if err != nil {
			return "", err
		}
		if under {
			return fresh, nil
		}
	}
	return "", &DenialError{Mode: p.Mode, Path: path}
}

var caseSensitive = runtime.GOOS != "windows" && runtime.GOOS != "darwin"

// isLexicallyUnder is the fast path for canonical spellings.
func isLexicallyUnder(path, root string) bool {
	p, r := path, root
	if !caseSensitive {
		p, r = strings.ToLower(p), strings.ToLower(r)
	}
	if p == r {
		return true
	}
	if !strings.HasSuffix(r, string(filepath.Separator)) {
		r += string(filepath.Separator)
	}
	return strings.HasPrefix(p, r)
}

// isPathUnder reports whether a canonical target is the root or lies beneath
// it (dsh's containment.ts). The lexical fast path handles canonical
// spellings; when spellings differ (Windows 8.3 aliases, casing), walk the
// target's existing ancestors and compare FILESYSTEM IDENTITY with the root
// via os.SameFile (dev+ino) — never weakening containment to text.
func isPathUnder(path, root string) (bool, error) {
	if isLexicallyUnder(path, root) {
		return true, nil
	}
	rootInfo, err := statIfPresent(root)
	if err != nil || rootInfo == nil {
		return false, err
	}
	ancestor := path
	for {
		info, err := statIfPresent(ancestor)
		if err != nil {
			return false, err
		}
		if info != nil && os.SameFile(info, rootInfo) {
			return true, nil
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return false, nil
		}
		ancestor = parent
	}
}

func statIfPresent(path string) (os.FileInfo, error) {
	info, err := os.Stat(path)
	if err == nil {
		return info, nil
	}
	if os.IsNotExist(err) {
		return nil, nil
	}
	return nil, err
}

// PromptSection renders the model-visible policy text (dsh's
// renderPolicyContext, capability-neutral wording). Confined modes only:
// danger-full-access contributes nothing to the prompt.
func PromptSection(p Policy) string {
	switch p.Mode {
	case ModeReadOnly:
		return "Current scode file policy: read-only. Any available operation enforced by the file sandbox cannot modify files in the standing mode. Do not refuse a required modification from this policy alone: try an available tool normally and follow any denial and escalation guidance it returns."
	case ModeWorkspaceWrite:
		return fmt.Sprintf("Current scode file policy: workspace-write. Any available operation enforced by the file sandbox may modify files under the session workspace: %q. The platform temporary area may also be writable.", p.WorkspaceRoot)
	default:
		return ""
	}
}
