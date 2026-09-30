package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseMode(t *testing.T) {
	for _, m := range Modes {
		if got, err := ParseMode(string(m)); err != nil || got != m {
			t.Fatalf("ParseMode(%q) = %v, %v", m, got, err)
		}
	}
	if _, err := ParseMode("yolo"); err == nil {
		t.Fatal("invalid mode should error")
	}
}

func TestWritableRoots(t *testing.T) {
	p := Policy{Mode: ModeReadOnly, WorkspaceRoot: "/w"}
	if len(p.WritableRoots()) != 0 {
		t.Fatal("read-only grants nothing")
	}
	p.Mode = ModeWorkspaceWrite
	roots := p.WritableRoots()
	if len(roots) < 1 || roots[0] != Canonical("/w") {
		t.Fatalf("roots = %v", roots)
	}
	found := false
	for _, r := range roots {
		if r == Canonical(os.TempDir()) {
			found = true
		}
	}
	if !found {
		t.Fatalf("temp dir missing from %v", roots)
	}
}

func TestCheckWrite(t *testing.T) {
	ws := t.TempDir()
	tmp := t.TempDir() // stand-in platform temp is os.TempDir(); test containment directly
	_ = tmp

	danger := Policy{Mode: ModeDangerFullAccess, WorkspaceRoot: ws}
	if _, err := danger.CheckWrite("C:/anywhere/x.txt"); err != nil {
		t.Fatal(err)
	}

	ro := Policy{Mode: ModeReadOnly, WorkspaceRoot: ws}
	if _, err := ro.CheckWrite(filepath.Join(ws, "x.txt")); err == nil {
		t.Fatal("read-only should deny")
	}

	ww := Policy{Mode: ModeWorkspaceWrite, WorkspaceRoot: ws}
	// inside workspace: allowed, returns canonical target
	fresh, err := ww.CheckWrite(filepath.Join(ws, "sub", "..", "x.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.ToLower(fresh), strings.ToLower(Canonical(ws))) && caseSensitive == false {
		t.Fatalf("fresh %q not under %q", fresh, ws)
	}
	// outside workspace (and outside the platform temp root): denied
	outside := filepath.Join(os.TempDir(), "..", "scode-definitely-outside.txt")
	if _, err := ww.CheckWrite(outside); err == nil {
		t.Fatal("outside workspace should deny")
	}
	// nonexistent file under workspace: allowed (canonical keeps spelling)
	if _, err := ww.CheckWrite(filepath.Join(ws, "new", "file.txt")); err != nil {
		t.Fatal(err)
	}
	// temp dir: allowed
	if _, err := ww.CheckWrite(filepath.Join(os.TempDir(), "scode-test-tmp.txt")); err != nil {
		t.Fatalf("temp should be writable: %v", err)
	}
}

// Windows long/8.3 alias and casing differences fall back to filesystem
// identity (os.SameFile) instead of text comparison.
func TestIsPathUnderCasingFallback(t *testing.T) {
	if caseSensitive {
		t.Skip("host filesystem is case-sensitive")
	}
	ws := t.TempDir()
	under, err := isPathUnder(filepath.Join(strings.ToUpper(ws), "x.txt"), ws)
	if err != nil || !under {
		t.Fatalf("casing alias: under=%v err=%v", under, err)
	}
}

func TestPromptSection(t *testing.T) {
	if s := PromptSection(Policy{Mode: ModeDangerFullAccess}); s != "" {
		t.Fatal("danger mode contributes nothing")
	}
	if s := PromptSection(Policy{Mode: ModeWorkspaceWrite, WorkspaceRoot: "/w"}); !strings.Contains(s, "/w") {
		t.Fatalf("workspace root missing: %q", s)
	}
	if s := PromptSection(Policy{Mode: ModeReadOnly}); !strings.Contains(s, "read-only") {
		t.Fatalf("read-only text: %q", s)
	}
}

// Fail-closed: an unresolved mode is a wiring bug, never permission. It must
// deny writes (and carry no escalation hint — there is no rung to climb).
func TestCheckWriteUnresolvedModeDenies(t *testing.T) {
	ws := t.TempDir()
	p := Policy{Mode: "", WorkspaceRoot: ws}
	if _, err := p.CheckWrite(filepath.Join(ws, "x.txt")); !errors.Is(err, ErrNoMode) {
		t.Fatalf("empty mode must fail closed, got: %v", err)
	}
	if _, err := (Policy{Mode: "yolo", WorkspaceRoot: ws}).CheckWrite(filepath.Join(ws, "x.txt")); !errors.Is(err, ErrNoMode) {
		t.Fatalf("unknown mode must fail closed, got: %v", err)
	}
	for _, m := range Modes {
		if !m.Valid() {
			t.Errorf("%q should be valid", m)
		}
	}
	for _, m := range []Mode{"", "yolo"} {
		if m.Valid() {
			t.Errorf("%q should be invalid", m)
		}
	}
}
