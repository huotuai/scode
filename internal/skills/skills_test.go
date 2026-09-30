package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkTree creates files under t.TempDir() and returns the root.
func mkTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const sampleSkill = `---
name: pdf-tools
description: Extract and merge PDF files
---
Use pdftk for everything.
`

func TestLoadFromDirSkillRoot(t *testing.T) {
	root := mkTree(t, map[string]string{
		"pdf/SKILL.md":   sampleSkill,
		"pdf/notes.md":   "not loaded (skill root stops recursion)",
		"other/SKILL.md": "---\nname: other\ndescription: Second skill\n---\nBody\n",
	})
	res := LoadFromDir(root, "project", ModePi)
	if len(res.Skills) != 2 {
		t.Fatalf("skills = %+v", res.Skills)
	}
	var s *Skill
	for i := range res.Skills {
		if res.Skills[i].Name == "pdf-tools" {
			s = &res.Skills[i]
		}
	}
	if s == nil || s.Description != "Extract and merge PDF files" {
		t.Fatalf("skill = %+v", res.Skills)
	}
	if s.BaseDir != filepath.Join(root, "pdf") || s.FilePath != filepath.Join(root, "pdf", "SKILL.md") {
		t.Fatalf("paths = %+v", s)
	}
}

func TestLoadFromDirModes(t *testing.T) {
	files := map[string]string{
		"loose.md":      "---\nname: loose\ndescription: Loose root file\n---\nBody\n",
		"sub/nested.md": "---\nname: nested\ndescription: Nested file\n---\nBody\n",
	}
	// pi mode: only ROOT-level bare .md counts (pi's includeRootFiles).
	root := mkTree(t, files)
	res := LoadFromDir(root, "user", ModePi)
	if len(res.Skills) != 1 || res.Skills[0].Name != "loose" {
		t.Fatalf("pi mode skills = %+v", res.Skills)
	}
	// agents mode: root .md excluded, nested .md counts.
	root = mkTree(t, files)
	res = LoadFromDir(root, "user", ModeAgents)
	if len(res.Skills) != 1 || res.Skills[0].Name != "nested" {
		t.Fatalf("agents mode skills = %+v", res.Skills)
	}
}

func TestLoadFromDirSkips(t *testing.T) {
	root := mkTree(t, map[string]string{
		".hidden/SKILL.md":        "---\ndescription: hidden\n---\n",
		"node_modules/x/SKILL.md": "---\ndescription: nm\n---\n",
		"plain.txt":               "no",
		"real/SKILL.md":           "---\ndescription: real\n---\n",
	})
	res := LoadFromDir(root, "project", ModePi)
	if len(res.Skills) != 1 || res.Skills[0].Description != "real" {
		t.Fatalf("skills = %+v", res.Skills)
	}
}

func TestLoadFromDirIgnoreFile(t *testing.T) {
	// agents mode so below-root .md files are discoverable; no SKILL.md
	// above them, or the skill-root rule would stop the scan.
	root := mkTree(t, map[string]string{
		".gitignore":       "ignored\nsub/blocked.md\n",
		"ignored/SKILL.md": "---\ndescription: skipped\n---\n",
		"kept/SKILL.md":    "---\ndescription: kept\n---\n",
		"sub/blocked.md":   "---\ndescription: blocked\n---\n",
		"sub/allowed.md":   "---\ndescription: allowed\n---\n",
	})
	res := LoadFromDir(root, "project", ModeAgents)
	if len(res.Skills) != 2 {
		t.Fatalf("skills = %+v", res.Skills)
	}
	descs := map[string]bool{}
	for _, s := range res.Skills {
		descs[s.Description] = true
	}
	if !descs["kept"] || !descs["allowed"] {
		t.Fatalf("skills = %+v", res.Skills)
	}
}

func TestLoadSkillFromFileRules(t *testing.T) {
	// Name falls back to the parent directory name.
	root := mkTree(t, map[string]string{
		"my-skill/SKILL.md": "---\ndescription: no name key\n---\nbody\n",
	})
	res := LoadFromDir(root, "user", ModePi)
	if len(res.Skills) != 1 || res.Skills[0].Name != "my-skill" {
		t.Fatalf("skills = %+v", res.Skills)
	}

	// Missing description: skill skipped (warning diagnostics).
	root = mkTree(t, map[string]string{
		"bad/SKILL.md": "---\nname: bad\n---\nbody\n",
	})
	res = LoadFromDir(root, "user", ModePi)
	if len(res.Skills) != 0 || len(res.Diagnostics) == 0 {
		t.Fatalf("res = %+v", res)
	}

	// Bare .md without description: skipped SILENTLY (pi).
	root = mkTree(t, map[string]string{"bare.md": "no frontmatter at all"})
	res = LoadFromDir(root, "user", ModePi)
	if len(res.Skills) != 0 || len(res.Diagnostics) != 0 {
		t.Fatalf("res = %+v", res)
	}

	// Invalid name: loads WITH a warning (pi keeps the skill).
	root = mkTree(t, map[string]string{
		"weird/SKILL.md": "---\nname: Weird_Name\ndescription: x\n---\n",
	})
	res = LoadFromDir(root, "user", ModePi)
	if len(res.Skills) != 1 || len(res.Diagnostics) == 0 {
		t.Fatalf("res = %+v", res)
	}

	// disable-model-invocation flag.
	root = mkTree(t, map[string]string{
		"manual/SKILL.md": "---\nname: manual\ndescription: x\ndisable-model-invocation: true\n---\n",
	})
	res = LoadFromDir(root, "user", ModePi)
	if len(res.Skills) != 1 || !res.Skills[0].DisableModelInvocation {
		t.Fatalf("res = %+v", res)
	}
}

func TestValidateName(t *testing.T) {
	cases := map[string]int{
		"pdf-tools":   0,
		"a":           0,
		"Weird":       1,
		"-lead":       1,
		"trail-":      1,
		"double--hy":  1,
		"with space":  1,
		"under_score": 1,
	}
	for name, wantErrs := range cases {
		if got := len(ValidateName(name)); got != wantErrs {
			t.Fatalf("ValidateName(%q) errors = %d, want %d", name, got, wantErrs)
		}
	}
	if len(ValidateName(strings.Repeat("a", 65))) == 0 {
		t.Fatal("65-char name must fail")
	}
}

func TestFormatForPrompt(t *testing.T) {
	skills := []Skill{
		{Name: "pdf-tools", Description: "PDF work & more", FilePath: "/x/pdf/SKILL.md"},
		{Name: "hidden", Description: "h", FilePath: "/x/h/SKILL.md", DisableModelInvocation: true},
	}
	out := FormatForPrompt(skills)
	if !strings.Contains(out, "<available_skills>") || !strings.Contains(out, "</available_skills>") {
		t.Fatalf("out = %q", out)
	}
	if !strings.Contains(out, "<name>pdf-tools</name>") {
		t.Fatalf("out = %q", out)
	}
	// XML escaping.
	if !strings.Contains(out, "PDF work &amp; more") {
		t.Fatalf("out = %q", out)
	}
	// disable-model-invocation skills are excluded.
	if strings.Contains(out, "hidden") {
		t.Fatalf("out = %q", out)
	}
	// Empty when nothing is visible.
	if FormatForPrompt([]Skill{{DisableModelInvocation: true}}) != "" {
		t.Fatal("want empty output")
	}
	if FormatForPrompt(nil) != "" {
		t.Fatal("want empty output")
	}
}

func TestExpandCommand(t *testing.T) {
	root := mkTree(t, map[string]string{
		"pdf/SKILL.md": sampleSkill,
	})
	res := LoadFromDir(root, "user", ModePi)
	skills := res.Skills

	// Non-skill input passes through.
	if got := ExpandCommand("hello", skills); got != "hello" {
		t.Fatalf("got = %q", got)
	}
	// Unknown skill passes through (pi sends it as a normal prompt).
	if got := ExpandCommand("/skill:nope do it", skills); got != "/skill:nope do it" {
		t.Fatalf("got = %q", got)
	}
	// Full expansion: block + relative note + frontmatter-stripped body.
	got := ExpandCommand("/skill:pdf-tools", skills)
	want := `<skill name="pdf-tools" location="` + filepath.Join(root, "pdf", "SKILL.md") + `">` + "\n" +
		"References are relative to " + filepath.Join(root, "pdf") + ".\n\n" +
		"Use pdftk for everything.\n</skill>"
	if got != want {
		t.Fatalf("got = %q\nwant = %q", got, want)
	}
	// Args append after the block.
	got = ExpandCommand("/skill:pdf-tools merge a.pdf b.pdf", skills)
	if !strings.HasSuffix(got, "</skill>\n\nmerge a.pdf b.pdf") {
		t.Fatalf("got = %q", got)
	}
}

func TestParseFrontmatter(t *testing.T) {
	fm, body, err := ParseFrontmatter(sampleSkill)
	if err != nil {
		t.Fatal(err)
	}
	if fm.Name != "pdf-tools" || fm.Description != "Extract and merge PDF files" {
		t.Fatalf("fm = %+v", fm)
	}
	if body != "Use pdftk for everything." {
		t.Fatalf("body = %q", body)
	}
	// No frontmatter: empty fm, whole content as body.
	fm, body, err = ParseFrontmatter("plain text")
	if err != nil || fm.Name != "" || body != "plain text" {
		t.Fatalf("fm=%+v body=%q err=%v", fm, body, err)
	}
	// CRLF + BOM tolerated (pi normalizes).
	fm, _, err = ParseFrontmatter("\uFEFF---\r\nname: x\r\n---\r\nbody")
	if err != nil || fm.Name != "x" {
		t.Fatalf("fm=%+v err=%v", fm, err)
	}
}

// An empty frontmatter block ("---\n---") must parse to an empty
// header + body, not panic: the raw slice normalized[4:3] had low >
// high, and loadSkillFromFile has no recover — one skeleton SKILL.md
// crashed the whole CLI at startup.
func TestParseFrontmatterEmptyHeader(t *testing.T) {
	for _, doc := range []string{"---\n---\nbody", "---\n---", "---\n---\n"} {
		fm, body, err := ParseFrontmatter(doc)
		if err != nil {
			t.Fatalf("ParseFrontmatter(%q): %v", doc, err)
		}
		if fm.Name != "" || fm.Description != "" {
			t.Fatalf("ParseFrontmatter(%q) header = %+v, want empty", doc, fm)
		}
		if doc == "---\n---\nbody" && body != "body" {
			t.Fatalf("body = %q, want %q", body, "body")
		}
	}
	// Sanity: the normal form still parses after the clamp.
	fm, body, err := ParseFrontmatter("---\nname: x\n---\nbody")
	if err != nil || fm.Name != "x" || body != "body" {
		t.Fatalf("normal form: fm=%+v body=%q err=%v", fm, body, err)
	}
}

func TestLoadAggregation(t *testing.T) {
	home := mkTree(t, map[string]string{
		".agents/skills/home-agents/SKILL.md": "---\ndescription: home agents skill\n---\n",
	})
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows

	agentDir := mkTree(t, map[string]string{
		"skills/global/SKILL.md": "---\ndescription: global user skill\n---\n",
		"skills/dupe/SKILL.md":   "---\nname: same\ndescription: user loses\n---\n",
	})
	project := mkTree(t, map[string]string{
		".git/HEAD":                            "ref: x",
		".scode/skills/local/SKILL.md":         "---\ndescription: project skill\n---\n",
		".agents/skills/agents-local/SKILL.md": "---\ndescription: project agents skill\n---\n",
		".scode/skills/dupe/SKILL.md":          "---\nname: same\ndescription: project wins\n---\n",
	})

	res := Load(Options{CWD: project, AgentDir: agentDir})
	names := map[string]string{}
	for _, s := range res.Skills {
		names[s.Name] = s.Description
	}
	for _, want := range []string{"project skill", "project agents skill", "global user skill", "home agents skill", "project wins"} {
		found := false
		for _, d := range names {
			if d == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing %q in %+v", want, names)
		}
	}
	if names["same"] != "project wins" {
		t.Fatalf("collision winner = %q", names["same"])
	}
	// Collision diagnostic recorded.
	var collision *Diagnostic
	for i := range res.Diagnostics {
		if res.Diagnostics[i].Type == "collision" {
			collision = &res.Diagnostics[i]
		}
	}
	if collision == nil || collision.WinnerPath == "" || collision.LoserPath == "" {
		t.Fatalf("diagnostics = %+v", res.Diagnostics)
	}

	// NoSkills: only explicit paths load.
	res = Load(Options{CWD: project, AgentDir: agentDir, NoSkills: true,
		CLIPaths: []string{filepath.Join(agentDir, "skills", "global", "SKILL.md")}})
	if len(res.Skills) != 1 || res.Skills[0].Description != "global user skill" {
		t.Fatalf("res = %+v", res.Skills)
	}
	// Explicit file under agentDir/skills classifies as "user".
	if res.Skills[0].Source != "user" {
		t.Fatalf("source = %q", res.Skills[0].Source)
	}

	// Missing path: warning diagnostic.
	res = Load(Options{CWD: project, AgentDir: agentDir, NoSkills: true, CLIPaths: []string{"does-not-exist"}})
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Type != "warning" {
		t.Fatalf("diagnostics = %+v", res.Diagnostics)
	}
}

// Fingerprint change detection (hot reload): identical trees hash equal;
// adding/editing/removing a .md file or a directory flips the token;
// non-.md files never influence discovery, so they don't flip it either.
func TestFingerprint(t *testing.T) {
	project := mkTree(t, map[string]string{
		".scode/skills/pdf/SKILL.md": sampleSkill,
	})
	opts := Options{CWD: project, AgentDir: t.TempDir(), NoSkills: false}

	fp1 := Fingerprint(opts)
	if fp1 != Fingerprint(opts) {
		t.Fatal("fingerprint not deterministic on an unchanged tree")
	}

	// A non-.md addition is invisible to discovery: same fingerprint.
	extra := filepath.Join(project, ".scode", "skills", "pdf", "note.txt")
	if err := os.WriteFile(extra, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Fingerprint(opts) != fp1 {
		t.Fatal("non-.md file changed the fingerprint")
	}

	// A new skill file flips it.
	added := filepath.Join(project, ".scode", "skills", "img", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(added), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(added, []byte(sampleSkill), 0o644); err != nil {
		t.Fatal(err)
	}
	fp2 := Fingerprint(opts)
	if fp2 == fp1 {
		t.Fatal("new SKILL.md did not change the fingerprint")
	}

	// An mtime/size change on an existing file flips it again.
	if err := os.WriteFile(added, []byte(sampleSkill+"\nmore\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Fingerprint(opts) == fp2 {
		t.Fatal("file edit did not change the fingerprint")
	}

	// NoSkills with an explicit file path: the path is still tracked.
	md := filepath.Join(project, ".scode", "skills", "pdf", "SKILL.md")
	fpA := Fingerprint(Options{CWD: project, AgentDir: t.TempDir(), NoSkills: true, CLIPaths: []string{md}})
	if err := os.WriteFile(md, []byte(sampleSkill+"\nedit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Fingerprint(Options{CWD: project, AgentDir: t.TempDir(), NoSkills: true, CLIPaths: []string{md}}) == fpA {
		t.Fatal("explicit-path edit did not change the fingerprint")
	}
}
