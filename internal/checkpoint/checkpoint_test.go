package checkpoint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureRestoreRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	file := filepath.Join(dir, "proj", "main.go")
	os.MkdirAll(filepath.Dir(file), 0o755)          //nolint:errcheck
	os.WriteFile(file, []byte("original\n"), 0o644) //nolint:errcheck

	// Capture the before-state, mutate.
	cp, err := s.Capture("sess1", "edit", "main.go", []string{file})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(file, []byte("mutated\n"), 0o644) //nolint:errcheck

	// A second file the edit CREATED: the capture runs BEFORE the write
	// lands (the decorator's order), so Existed=false — restore deletes it.
	created := filepath.Join(dir, "proj", "new.go")
	cp2, err := s.Capture("sess1", "write", "new.go", []string{created})
	if err != nil {
		t.Fatal(err)
	}
	if cp2.Files[0].Existed {
		t.Fatal("not-yet-created file recorded as pre-existing")
	}
	os.WriteFile(created, []byte("new\n"), 0o644) //nolint:errcheck

	// List is newest first, ids are sequential.
	cps := s.List("sess1")
	if len(cps) != 2 || cps[0].ID != cp2.ID || cps[1].ID != cp.ID {
		t.Fatalf("list order = %+v", cps)
	}

	// Restore the FIRST checkpoint: main.go returns to original.
	if n, err := s.Restore("sess1", cp.ID); err != nil || n != 1 {
		t.Fatalf("restore = %d %v", n, err)
	}
	data, _ := os.ReadFile(file)
	if string(data) != "original\n" {
		t.Fatalf("restore wrote %q", data)
	}

	// Restore the second: the created file disappears again.
	if _, err := s.Restore("sess1", cp2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatal("created file survived its restore")
	}

	// Remove drops a checkpoint; unknown restores error.
	s.Remove("sess1", cp.ID)
	if cps = s.List("sess1"); len(cps) != 1 {
		t.Fatalf("remove left %+v", cps)
	}
	if _, err := s.Restore("sess1", cp.ID); err == nil {
		t.Fatal("restoring a removed checkpoint should error")
	}
}

func TestPruneKeepsCap(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	file := filepath.Join(dir, "x.txt")
	os.WriteFile(file, []byte("v\n"), 0o644) //nolint:errcheck
	for i := 0; i < KeepPerSession+10; i++ {
		if _, err := s.Capture("sess", "edit", "x.txt", []string{file}); err != nil {
			t.Fatal(err)
		}
	}
	cps := s.List("sess")
	if len(cps) > KeepPerSession {
		t.Fatalf("kept %d, want ≤ %d", len(cps), KeepPerSession)
	}
	// The pruned ids' blob dirs are gone too.
	entries, _ := os.ReadDir(filepath.Join(s.Root, "sess"))
	dirs := 0
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "c") {
			dirs++
		}
	}
	if dirs > KeepPerSession {
		t.Fatalf("blob dirs = %d, want ≤ %d", dirs, KeepPerSession)
	}
	// The newest checkpoints survive (restorable).
	if n, err := s.Restore("sess", cps[0].ID); err != nil || n != 1 {
		t.Fatalf("newest not restorable: %d %v", n, err)
	}
}

func TestSessionsAreIsolated(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	file := filepath.Join(dir, "x.txt")
	os.WriteFile(file, []byte("1\n"), 0o644) //nolint:errcheck
	if _, err := s.Capture("a", "edit", "x.txt", []string{file}); err != nil {
		t.Fatal(err)
	}
	if cps := s.List("b"); len(cps) != 0 {
		t.Fatalf("session b sees session a's points: %+v", cps)
	}
}

// The size guard: files over MaxSnapshotBytes are recorded as TooLarge
// (no blob — no seconds-long copy, no gigabytes across the cap);
// restore skips them; mixed checkpoints keep their small files.
func TestSizeGuard(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	big := filepath.Join(dir, "big.log")
	os.WriteFile(big, make([]byte, MaxSnapshotBytes+1), 0o644) //nolint:errcheck
	small := filepath.Join(dir, "small.go")
	os.WriteFile(small, []byte("v1\n"), 0o644) //nolint:errcheck

	// A checkpoint of ONLY the oversized file still records the point.
	cpBig, err := s.Capture("sess", "write", "big.log", []string{big})
	if err != nil {
		t.Fatal(err)
	}
	if !cpBig.Files[0].TooLarge || !cpBig.Files[0].Existed {
		t.Fatalf("oversized snap = %+v", cpBig.Files[0])
	}
	// No blob was written.
	entries, _ := os.ReadDir(filepath.Join(s.Root, "sess", cpBig.ID))
	if len(entries) != 0 {
		t.Fatalf("oversized file left blobs: %v", entries)
	}
	// Restore touches nothing (and does not error).
	if n, err := s.Restore("sess", cpBig.ID); err != nil || n != 0 {
		t.Fatalf("oversized restore = %d %v", n, err)
	}

	// Mixed: the small file snapshots normally, the big one flags.
	cpMix, err := s.Capture("sess", "write", "mixed", []string{big, small})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(small, []byte("v2\n"), 0o644) //nolint:errcheck
	if n, err := s.Restore("sess", cpMix.ID); err != nil || n != 1 {
		t.Fatalf("mixed restore = %d %v", n, err)
	}
	data, _ := os.ReadFile(small)
	if string(data) != "v1\n" {
		t.Fatalf("small file not restored: %q", data)
	}
}
