package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scode/internal/llm"
	"scode/internal/skills"
)

// Skill hot reload: a skill added AFTER Setup is invisible to
// $name expansion until /reload — then it expands and joins the
// folded system prompt; a later REMOVAL is picked up lazily by the
// failed-lookup reload (no per-prompt rescan anywhere).
func TestSkillHotReload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatSSE(w, "unused", 100)
	}))
	defer srv.Close()
	setupTestApp(t, srv)

	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck

	skillDir := filepath.Join(app.CWD, ".scode", "skills", "hot")
	skillFile := filepath.Join(skillDir, "SKILL.md")
	writeSkill := func() {
		t.Helper()
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: hot\ndescription: added mid-session\n---\n\nHOT BODY\n"
		if err := os.WriteFile(skillFile, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Baseline: the skill does not exist; the command passes through.
	if got := skills.ExpandCommand("$hot do it", app.skillList()); got != "$hot do it" {
		t.Fatalf("baseline expansion = %q", got)
	}

	// Added mid-session: still unknown until a reload.
	writeSkill()
	if got := skills.ExpandCommand("$hot do it", app.skillList()); got != "$hot do it" {
		t.Fatalf("pre-reload expansion = %q", got)
	}

	// Manual /reload: the command reports the addition...
	out, done, err := app.Command("/reload")
	if err != nil || done {
		t.Fatalf("/reload = %q, %v, %v", out, done, err)
	}
	if !strings.Contains(out, "added: hot") {
		t.Fatalf("/reload summary = %q", out)
	}
	// ...$name expands against the new set...
	if got := skills.ExpandCommand("$hot do it", app.skillList()); !strings.Contains(got, `<skill name="hot"`) || !strings.HasSuffix(got, "do it") {
		t.Fatalf("post-reload expansion = %q", got)
	}
	// ...and the folded system prompt lists it for the next request.
	folded := llm.CurrentSystemMessage(app.Tr.Messages())
	if folded == nil {
		t.Fatal("no folded system message")
	}
	listed := false
	for _, s := range folded.Sections {
		if s.Name == "skills" && strings.Contains(s.Value, "<name>hot</name>") {
			listed = true
		}
	}
	if !listed {
		t.Fatal("folded system prompt does not list the reloaded skill")
	}

	// A second /reload with an unchanged tree reports no changes.
	out, _, err = app.Command("/reload")
	if err != nil || !strings.Contains(out, "no changes") {
		t.Fatalf("second /reload = %q, %v", out, err)
	}

	// Removal is picked up LAZILY: the next $hot lookup fails, the
	// fingerprint check notices the changed tree, reloads, and retries —
	// no manual command, no per-prompt rescan.
	if err := os.RemoveAll(filepath.Join(app.CWD, ".scode", "skills")); err != nil {
		t.Fatal(err)
	}
	if got := app.expandSkill("$hot do it"); got != "$hot do it" {
		t.Fatalf("post-removal expansion = %q", got)
	}
	folded = llm.CurrentSystemMessage(app.Tr.Messages())
	for _, s := range folded.Sections {
		if s.Name == "skills" && strings.Contains(s.Value, "<name>hot</name>") {
			t.Fatal("folded system prompt still lists the removed skill")
		}
	}

	// The reverse direction is lazy too: re-add the skill and the next
	// $hot lookup reloads + expands with NO manual /reload.
	writeSkill()
	if got := app.expandSkill("$hot do it"); !strings.Contains(got, `<skill name="hot"`) {
		t.Fatalf("lazy-add expansion = %q", got)
	}
}
