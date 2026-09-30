package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreApplyRoundtrip(t *testing.T) {
	s := Open(t.TempDir(), "/proj/x")

	if entries, err := s.Load(); err != nil || len(entries) != 0 {
		t.Fatalf("absent store: %v %v", entries, err)
	}

	// Adds dedupe by content; categories normalize.
	a, u, d := s.Apply([]Op{
		{Op: "add", Category: "project", Content: "构建: go build ./..."},
		{Op: "add", Category: "bogus", Content: "偏好: 回复用中文"},
		{Op: "add", Category: "project", Content: "构建: go build ./..."}, // duplicate content
	}, "sess1")
	if a != 2 || u != 1 || d != 0 {
		t.Fatalf("apply counts = %d/%d/%d (2 adds + 1 dedupe-update)", a, u, d)
	}
	entries, _ := s.Load()
	if len(entries) != 2 {
		t.Fatalf("dedupe failed: %+v", entries)
	}
	for _, e := range entries {
		if e.ID == "" {
			t.Fatalf("empty id: %+v", e)
		}
		if e.Content == "偏好: 回复用中文" && e.Category != CatGeneral {
			t.Fatalf("bogus category not normalized: %+v", e)
		}
		if e.Content == "构建: go build ./..." && e.Category != CatProject {
			t.Fatalf("project category lost: %+v", e)
		}
	}
	if entries[0].ID == entries[1].ID {
		t.Fatalf("id collision: %+v", entries)
	}

	// Update by id, delete by id.
	id := entries[1].ID
	a, u, d = s.Apply([]Op{
		{Op: "update", ID: id, Content: "构建: go build ./... (含 vet)"},
		{Op: "delete", ID: entries[0].ID},
		{Op: "update", ID: "missing", Content: "x"}, // unknown id skipped
	}, "sess2")
	if a != 0 || u != 1 || d != 1 {
		t.Fatalf("apply counts = %d/%d/%d", a, u, d)
	}
	entries, _ = s.Load()
	if len(entries) != 1 || entries[0].Content != "构建: go build ./... (含 vet)" {
		t.Fatalf("update/delete wrong: %+v", entries)
	}

	// The cap: MaxEntries+5 adds keep the newest MaxEntries.
	var ops []Op
	for i := 0; i < MaxEntries+5; i++ {
		ops = append(ops, Op{Op: "add", Category: "fact", Content: "fact " + strings.Repeat("x", i%7) + itoa(i)})
	}
	s.Apply(ops, "sess3")
	if entries, _ = s.Load(); len(entries) > MaxEntries {
		t.Fatalf("cap exceeded: %d", len(entries))
	}

	// Toggle + Delete wrappers operate on the capped list.
	if entries, _ = s.Load(); len(entries) != MaxEntries {
		t.Fatalf("capped list = %d, want %d", len(entries), MaxEntries)
	}
	on, err := s.ToggleSelected(entries[0].ID)
	if err != nil || !on {
		t.Fatalf("toggle = %v %v", on, err)
	}
	if err := s.Delete(entries[0].ID); err != nil {
		t.Fatal(err)
	}
	if after, _ := s.Load(); len(after) != MaxEntries-1 {
		t.Fatalf("delete wrapper left %d", len(after))
	}
}

func itoa(i int) string {
	return string(rune('a' + i%26))
}

func TestFormatForPrompt(t *testing.T) {
	entries := []Entry{
		{ID: "1", Category: CatProject, Content: "构建用 build.bat"},
		{ID: "2", Category: CatPreference, Content: "回复用中文"},
		{ID: "3", Category: CatFact, Content: "Windows 剪贴板走 user32"},
		{ID: "4", Category: CatGeneral, Content: "未分类事实"},
	}

	// Typed: grouped with labels.
	typed := FormatForPrompt(entries, true, false)
	for _, want := range []string{"项目:", "偏好:", "事实:", "通用:", "构建用 build.bat", "回复用中文"} {
		if !strings.Contains(typed, want) {
			t.Fatalf("typed format missing %q:\n%s", want, typed)
		}
	}

	// Untyped: flat bullets, no category headers.
	flat := FormatForPrompt(entries, false, false)
	if strings.Contains(flat, "项目:") || !strings.Contains(flat, "- 构建用 build.bat") {
		t.Fatalf("flat format wrong:\n%s", flat)
	}

	// Relevance: only hand-picked entries.
	entries[1].Selected = true
	rel := FormatForPrompt(entries, true, true)
	if !strings.Contains(rel, "回复用中文") || strings.Contains(rel, "构建用 build.bat") {
		t.Fatalf("relevance filter wrong:\n%s", rel)
	}

	// Everything filtered out → no section.
	entries[1].Selected = false
	if got := FormatForPrompt(entries, true, true); got != "" {
		t.Fatalf("empty relevance should yield no section: %q", got)
	}
	if got := FormatForPrompt(nil, true, false); got != "" {
		t.Fatalf("nil entries should yield no section: %q", got)
	}
}

func TestStorePathScoping(t *testing.T) {
	// The replacer turns ":\\" into "--" (both sanitized), matching the
	// session store's directory names.
	if p := StorePath("cfg", `E:\ai\harness\SCode`); filepath.Base(p) != "E--ai-harness-SCode.json" {
		t.Fatalf("path = %q", p)
	}
	if _, err := os.Stat(StorePath("cfg", "/x")); !os.IsNotExist(err) {
		t.Fatal("store file created eagerly")
	}
}
