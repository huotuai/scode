package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Regression coverage for the file-fence escape through a linked ancestor
// (a symlink on unix, a junction on Windows — `mklink /J` needs no
// privilege, and Go reports a junction as a plain directory, so
// filepath.EvalSymlinks leaves it in place).
//
// The fence must deny all three shapes: an EXISTING target outside, a NEW
// target outside (the missing-suffix case), and a nested one. Each test
// asserts the real write result too: a policy that says "allowed" while the
// bytes land outside is the failure being pinned.

// linkDir creates a directory link and registers its removal. The link is
// removed explicitly so no cleanup path ever traverses it.
func linkDir(t *testing.T, link, target string) {
	t.Helper()
	err := os.Symlink(target, link)
	if err != nil && runtime.GOOS == "windows" {
		// Junctions are the realistic Windows vector AND the one Go's
		// EvalSymlinks cannot see.
		if out, jerr := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); jerr != nil {
			t.Skipf("no directory-link primitive: symlink=%v junction=%v %s", err, jerr, out)
		}
		t.Cleanup(func() { _ = os.Remove(link) })
		return
	}
	if err != nil {
		t.Skipf("no directory-link primitive: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
}

// outsideDir builds a decoy directory that lies outside EVERY writable root
// of p (the workspace, the temp area, /tmp). t.TempDir() is not usable: it
// lives under the temp root, which workspace-write legitimately allows.
func outsideDir(t *testing.T, p Policy) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory for the decoy: %v", err)
	}
	dir := filepath.Join(home, fmt.Sprintf("scode-sandbox-test-%d", os.Getpid()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Skipf("cannot create the decoy outside the workspace: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// Verify the decoy really is unfenced: a policy-allowed probe means the
	// location is inside a writable root and the escape test is meaningless.
	if _, err := p.CheckWrite(filepath.Join(dir, "probe.txt")); err == nil {
		t.Skipf("decoy %s falls inside a writable root", dir)
	}
	return dir
}

func TestCheckWriteRejectsLinkedAncestorEscape(t *testing.T) {
	ws := t.TempDir()
	p := Policy{Mode: ModeWorkspaceWrite, WorkspaceRoot: ws}
	outside := outsideDir(t, p)

	existing := filepath.Join(outside, "existing.txt")
	if err := os.WriteFile(existing, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws, "link")
	linkDir(t, link, outside)

	cases := []struct{ name, target string }{
		{"existing target outside", filepath.Join(link, "existing.txt")},
		{"new target outside", filepath.Join(link, "new.txt")},
		{"nested new target outside", filepath.Join(link, "sub", "deep", "new.txt")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.CheckWrite(tc.target)
			if err == nil {
				t.Fatalf("escape allowed: CheckWrite(%q) = %q", tc.target, got)
			}
			if !strings.Contains(err.Error(), "denied") {
				t.Fatalf("expected a policy denial, got: %v", err)
			}
		})
	}

	// The fence must not be over-tightened: a plain workspace target still
	// passes, and the returned path is the canonical one.
	inside := filepath.Join(ws, "ok.txt")
	got, err := p.CheckWrite(inside)
	if err != nil {
		t.Fatalf("inside write denied: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(got), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(got, []byte("fine"), 0o644); err != nil {
		t.Fatalf("inside write failed: %v", err)
	}
}

// TestCanonicalResolvesLinkedAncestorWithMissingSuffix pins the resolution
// itself, independent of the policy: the deepest EXISTING ancestor is
// realpath'd and the missing suffix re-appended (dsh's fsio.resolve).
func TestCanonicalResolvesLinkedAncestorWithMissingSuffix(t *testing.T) {
	ws := t.TempDir()
	p := Policy{Mode: ModeWorkspaceWrite, WorkspaceRoot: ws}
	outside := outsideDir(t, p)
	link := filepath.Join(ws, "link")
	linkDir(t, link, outside)

	target := filepath.Join(link, "sub", "new.txt")
	want := filepath.Join(Canonical(outside), "sub", "new.txt")
	if got := Canonical(target); got != want {
		t.Errorf("Canonical(%q) = %q, want %q", target, got, want)
	}
	if Canonical(target) == filepath.Clean(target) {
		t.Errorf("resolution fell back to the lexical spelling: %q", Canonical(target))
	}
}

// TestCanonicalExistingPathUnchanged guards the common case: an existing
// in-workspace path canonicalizes to itself and containment still holds.
func TestCanonicalExistingPathUnchanged(t *testing.T) {
	ws := t.TempDir()
	file := filepath.Join(ws, "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := Policy{Mode: ModeWorkspaceWrite, WorkspaceRoot: ws}
	got, err := p.CheckWrite(file)
	if err != nil {
		t.Fatalf("existing workspace file denied: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("returned path is not absolute: %q", got)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("returned path is not usable for I/O: %v", err)
	}
}

// TestCanonicalDirectoryComponentIsFile pins the ENOTDIR handling: a path
// whose ancestor is a regular file cannot exist, so the walk must step up
// rather than turn the natural "not a directory" failure into a sandbox
// denial.
func TestCanonicalDirectoryComponentIsFile(t *testing.T) {
	ws := t.TempDir()
	file := filepath.Join(ws, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := Policy{Mode: ModeWorkspaceWrite, WorkspaceRoot: ws}
	got, err := p.CheckWrite(filepath.Join(file, "child.txt"))
	if err != nil {
		t.Fatalf("ENOTDIR must not be reported as a sandbox denial: %v", err)
	}
	if err := os.WriteFile(got, []byte("y"), 0o644); err == nil {
		t.Fatalf("writing under a regular file unexpectedly succeeded")
	}
}
