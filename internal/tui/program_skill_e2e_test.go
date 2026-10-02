//go:build windows || linux || darwin

package tui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"scode/internal/cli"
)

// End-to-end: typing "$name args" in the TUI expands the skill
// at the prompt boundary (pi's _expandSkillCommand) — the provider
// request must carry the <skill> block with the file body and trailing
// args. Args are optional: "$name" alone expands the same way.
func TestProgramSkillCommand(t *testing.T) {
	reqBodies := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reqBodies <- string(b)
		chatSSEBlob(w, "ok")
	}))
	defer srv.Close()
	cfgDir := t.TempDir()
	proj := t.TempDir()
	settings := map[string]any{
		"defaultProvider": "openai-compat",
		"sandbox":         map[string]any{"mode": "danger-full-access"},
		"providers": map[string]any{
			"openai-compat": map[string]any{"apiKey": "k", "baseUrl": srv.URL, "model": "m"},
		},
	}
	sb, _ := json.Marshal(settings)
	os.WriteFile(filepath.Join(cfgDir, "settings.json"), sb, 0o644) //nolint:errcheck
	skillDir := filepath.Join(proj, ".scode", "skills", "code-review")
	os.MkdirAll(skillDir, 0o755)                                                                                                                    //nolint:errcheck
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: code-review\ndescription: review a PR\n---\n\nREVIEW BODY HERE\n"), 0o644) //nolint:errcheck
	t.Setenv("SCODE_DIR", cfgDir)
	t.Chdir(proj)

	ui := make(chan any, 256)
	app, err := cli.Setup(cli.Options{Approver: newApprover(ui)})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck

	pr, pw := io.Pipe()
	out := &syncWriter{}
	p := tea.NewProgram(newModel(app, ui),
		tea.WithInput(pr),
		tea.WithOutput(out),
		tea.WithWindowSize(100, 30),
		tea.WithoutSignals(),
	)
	runDone := make(chan error, 1)
	go func() { _, err := p.Run(); runDone <- err }()

	if err := (scriptWriter{pw}).write("$code-review 看一下这个PR\r"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var body string
	for body == "" {
		if time.Now().After(deadline) {
			t.Fatalf("provider never called; output:\n%s", stripANSI(out.String()))
		}
		select {
		case body = <-reqBodies:
		case err := <-runDone:
			t.Fatalf("program exited early: %v; output:\n%s", err, stripANSI(out.String()))
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
	for _, want := range []string{`skill name=`, "REVIEW BODY HERE", "看一下这个PR"} {
		if !strings.Contains(body, want) {
			t.Errorf("request missing %q; body:\n%s", want, body)
		}
	}
	time.Sleep(200 * time.Millisecond)
	quitProgram(t, pw, runDone)
}
