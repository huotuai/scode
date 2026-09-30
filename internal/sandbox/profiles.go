// Platform profile builders (dsh's sandbox-local/profiles.ts): one policy
// expressed in each enforcement dialect's argv/string vocabulary. Kept
// untagged so the dialect bytes stay unit-testable from any host — the
// darwin and linux spawners consume these verbatim.
package sandbox

import (
	"fmt"
	"strings"
)

// BwrapProfileArgs builds bubblewrap's profile for one policy (everything
// before the trailing "--" and the wrapped argv). Read-only ro-binds the
// whole filesystem; workspace-write overlays a private tmpfs /tmp and an
// rw bind of the workspace. `--die-with-parent` keeps a killed serve
// process from leaking its confined descendants.
func BwrapProfileArgs(p Policy) []string {
	args := []string{"--ro-bind", "/", "/", "--dev", "/dev", "--unshare-pid", "--proc", "/proc", "--die-with-parent"}
	if p.Mode == ModeWorkspaceWrite {
		ws := Canonical(p.WorkspaceRoot)
		args = append(args, "--tmpfs", "/tmp", "--bind", ws, ws)
	}
	return args
}

// LandlockGrantArgs builds the landlock launcher's allow-list: the
// filesystem read-only, /dev/null always writable, and the mode's writable
// roots under workspace-write (Landlock is a strict allow-list, so anything
// unnamed is denied). The roots come from WritableRoots — the same
// derivation the in-process fs fence uses, so the two cannot drift.
func LandlockGrantArgs(p Policy) []string {
	args := []string{"--ro", "/", "--rw", "/dev/null"}
	if p.Mode == ModeWorkspaceWrite {
		for _, root := range p.WritableRoots() {
			args = append(args, "--rw", root)
		}
	}
	return args
}

// SeatbeltProfile builds the SBPL profile for one policy (the -p argument
// to sandbox-exec): allow everything, deny every file write, then re-allow
// the mode's writable roots by subpath. Root derivation is shared with the
// fs fence (WritableRoots), so Seatbelt and the in-process check never
// disagree — dsh's explicit invariant.
func SeatbeltProfile(p Policy) string {
	forms := []string{
		"(version 1)",
		"(allow default)",
		"(deny file-write*)",
		fmt.Sprintf("(allow file-write* (literal %s))", sbplString("/dev/null")),
	}
	if roots := p.WritableRoots(); len(roots) > 0 {
		subs := make([]string, 0, len(roots))
		for _, root := range roots {
			subs = append(subs, fmt.Sprintf("(subpath %s)", sbplString(root)))
		}
		forms = append(forms, "(allow file-write* "+strings.Join(subs, " ")+")")
	}
	return strings.Join(forms, " ")
}

// sbplString quotes one path as an SBPL string literal (dsh's sbplString:
// backslashes first, then quotes).
func sbplString(path string) string {
	path = strings.ReplaceAll(path, `\`, `\\`)
	return `"` + strings.ReplaceAll(path, `"`, `\"`) + `"`
}
