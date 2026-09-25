package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scode/internal/llm"
)

func TestFindInstructionsOrder(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	os.MkdirAll(filepath.Join(proj, "sub"), 0o755) //nolint:errcheck
	cfg := t.TempDir()

	os.WriteFile(filepath.Join(cfg, "AGENTS.md"), []byte("global"), 0o644)               //nolint:errcheck
	os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("repo"), 0o644)                //nolint:errcheck
	os.WriteFile(filepath.Join(proj, "CLAUDE.md"), []byte("project"), 0o644)             //nolint:errcheck
	os.WriteFile(filepath.Join(proj, "AGENTS.md"), []byte("shadowed-by-none"), 0o644)    //nolint:errcheck
	os.WriteFile(filepath.Join(proj, "sub", "AGENTS.override.md"), []byte("sub"), 0o644) //nolint:errcheck

	// cwd = proj/sub: global first, then root, then proj, then sub.
	// In proj both AGENTS.md and CLAUDE.md exist — AGENTS.md wins per the
	// candidate order.
	got := FindInstructions(cfg, filepath.Join(proj, "sub"))
	var contents []string
	for _, p := range got {
		b, _ := os.ReadFile(p)
		contents = append(contents, string(b))
	}
	want := []string{"global", "repo", "shadowed-by-none", "sub"}
	if len(contents) != len(want) {
		t.Fatalf("got %v", contents)
	}
	for i := range want {
		if contents[i] != want[i] {
			t.Fatalf("order = %v, want %v", contents, want)
		}
	}
}

func TestFindInstructionsPerDirFirstWins(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("claude"), 0o644) //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("agents"), 0o644) //nolint:errcheck
	got := FindInstructions(t.TempDir(), dir)
	if len(got) != 1 || strings.HasSuffix(got[0], "CLAUDE.md") {
		t.Fatalf("got %v, want AGENTS.md to win over CLAUDE.md", got)
	}
}

func TestBuildStableSections(t *testing.T) {
	cfg := t.TempDir()
	proj := t.TempDir()
	os.WriteFile(filepath.Join(proj, "AGENTS.md"), []byte("use Go 1.26"), 0o644) //nolint:errcheck

	m := Build(cfg, proj, nil)
	if m.Role != llm.RoleSystem || m.TS != 0 {
		t.Fatalf("message = %+v", m)
	}
	if len(m.Content) != 1 || m.Content[0].Text != preamble {
		t.Fatalf("preamble wrong")
	}
	var names []string
	values := map[string]string{}
	for _, s := range m.Sections {
		names = append(names, s.Name)
		values[s.Name] = s.Value
	}
	want := []string{"rules", "cwd", "project_context"}
	if len(names) != len(want) {
		t.Fatalf("sections = %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("section order = %v, want %v", names, want)
		}
	}
	if !strings.Contains(values["project_context"], "use Go 1.26") {
		t.Fatalf("project_context = %q", values["project_context"])
	}
	if !strings.Contains(values["project_context"], "<project_instructions") {
		t.Fatal("instruction wrapper missing")
	}
	// Cache rule: no volatile content in the scode-controlled sections.
	controlled := m.Content[0].Text + rulesText + values["cwd"]
	if strings.Contains(controlled, "2026") || strings.Contains(controlled, "git status") {
		t.Fatal("volatile content leaked into the prompt")
	}
}
