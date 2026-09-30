package sandbox

import (
	"strings"
	"testing"
)

// The dialect bytes are contracts (each runner parses them), so they are
// pinned exactly — a refactor that reorders a mount or drops a flag must be
// a deliberate, visible test change.

func TestBwrapProfileArgs(t *testing.T) {
	const base = "--ro-bind / / --dev /dev --unshare-pid --proc /proc --die-with-parent"
	ro := BwrapProfileArgs(Policy{Mode: ModeReadOnly, WorkspaceRoot: "/ws"})
	if got := strings.Join(ro, " "); got != base {
		t.Errorf("read-only profile = %q, want %q", got, base)
	}
	ww := BwrapProfileArgs(Policy{Mode: ModeWorkspaceWrite, WorkspaceRoot: "/ws"})
	if got, want := strings.Join(ww, " "), base+" --tmpfs /tmp --bind "+Canonical("/ws")+" "+Canonical("/ws"); got != want {
		t.Errorf("workspace-write profile = %q, want %q", got, want)
	}
}

func TestLandlockGrantArgs(t *testing.T) {
	ro := LandlockGrantArgs(Policy{Mode: ModeReadOnly, WorkspaceRoot: "/ws"})
	if got, want := strings.Join(ro, " "), "--ro / --rw /dev/null"; got != want {
		t.Errorf("read-only grants = %q, want %q", got, want)
	}
	ws := t.TempDir()
	ww := LandlockGrantArgs(Policy{Mode: ModeWorkspaceWrite, WorkspaceRoot: ws})
	if len(ww) < 4 || ww[0] != "--ro" || ww[1] != "/" {
		t.Fatalf("grants must open with the read-only root: %q", ww)
	}
	// Every --rw path is a declared writable root, and the workspace is
	// among them (allow-list completeness).
	allowed := map[string]bool{}
	for _, r := range (Policy{Mode: ModeWorkspaceWrite, WorkspaceRoot: ws}).WritableRoots() {
		allowed[r] = true
	}
	if !allowed[Canonical(ws)] {
		t.Fatalf("workspace missing from writable roots: %v", allowed)
	}
	for i := 0; i < len(ww); i += 2 {
		if ww[i] != "--rw" {
			continue
		}
		if !allowed[ww[i+1]] && ww[i+1] != "/dev/null" {
			t.Errorf("granted root %q is not a writable root", ww[i+1])
		}
	}
}

func TestSeatbeltProfile(t *testing.T) {
	ro := SeatbeltProfile(Policy{Mode: ModeReadOnly, WorkspaceRoot: "/ws"})
	if !strings.HasPrefix(ro, "(version 1) (allow default) (deny file-write*) (allow file-write* (literal \"/dev/null\"))") {
		t.Fatalf("profile prefix = %q", ro)
	}
	if strings.Contains(ro, "(subpath") {
		t.Errorf("read-only must grant no writable subpath: %q", ro)
	}
	ws := t.TempDir()
	ww := SeatbeltProfile(Policy{Mode: ModeWorkspaceWrite, WorkspaceRoot: ws})
	if !strings.Contains(ww, `(allow file-write* (subpath `+sbplString(Canonical(ws))+`)`) {
		t.Errorf("workspace-write must grant the workspace subpath: %q", ww)
	}
	if !strings.Contains(ww, "(literal \"/dev/null\")") {
		t.Errorf("profile must keep the /dev/null literal: %q", ww)
	}
}

func TestSbplStringEscaping(t *testing.T) {
	if got := sbplString(`C:\a"b`); got != `"C:\\a\"b"` {
		t.Errorf("sbplString = %q", got)
	}
}
