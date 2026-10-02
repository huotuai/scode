package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"scode/internal/llm"
)

func TestIsPromptCommand(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"/commit", true},
		{"/commit focus the tests", true},
		{"/commit-push-pr", true},
		{"/commit-push-pr after the rebase", true},
		{"/committing", false},         // name must end or be followed by a space
		{"/commit-x", false},           // …and /commit-x is not /commit
		{"/compact", false},            // other builtins are not prompt commands
		{"/skill:commit", false},       // skill namespace
		{"commit", false},              // not a slash line
		{"/COMMIT", false},             // case-sensitive like App.Command
		{"", false},                    // empty
		{"/commit ", true},             // trailing space: empty instructions
		{"/commit-push-prx", false},    // longest name still needs the boundary
		{"/commit --amend only", true}, // leading-dash args are instructions
		{"/init", true},
		{"/init focus on commands", true},
		{"/initial", false}, // boundary: not /init
	}
	for _, c := range cases {
		if got := IsPromptCommand(c.line); got != c.want {
			t.Errorf("IsPromptCommand(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestExpandPromptCommand(t *testing.T) {
	// Exact invocation → the canned body, nothing else.
	body, ok := expandPromptCommand("/commit")
	if !ok || !strings.Contains(body, "Create a git commit for the current changes.") {
		t.Fatalf("expand /commit = %q, ok=%v", body, ok)
	}
	// Longest name wins: /commit-push-pr must not expand the /commit body.
	body, ok = expandPromptCommand("/commit-push-pr")
	if !ok || !strings.Contains(body, "open a pull request") {
		t.Fatalf("expand /commit-push-pr = %q, ok=%v", body, ok)
	}
	if strings.Contains(body, "Create a git commit for the current changes.") {
		t.Fatal("/commit-push-pr expanded the /commit body — longest-name match broken")
	}
	// Trailing instructions ride along.
	body, ok = expandPromptCommand("/commit  focus the message on perf ")
	if !ok || !strings.Contains(body, "Additional instructions from the user: focus the message on perf") {
		t.Fatalf("instructions not appended: %q", body)
	}
	if !strings.Contains(body, "Create a git commit") {
		t.Fatal("canned body missing alongside instructions")
	}
	// Non-commands pass through flagged as unmatched.
	body, ok = expandPromptCommand("just a prompt")
	if ok || body != "just a prompt" {
		t.Fatalf("passthrough broken: %q, ok=%v", body, ok)
	}

	// /init carries the Codex-format AGENTS.md template.
	body, ok = expandPromptCommand("/init")
	if !ok || !strings.Contains(body, "AGENTS.md") || !strings.Contains(body, "## Project overview") ||
		!strings.Contains(body, "## Building and running") || !strings.Contains(body, "## Testing") ||
		!strings.Contains(body, "## Code style") || !strings.Contains(body, "## Architecture") {
		t.Fatalf("/init body missing the Codex structure:\n%s", truncateFor(body, 300))
	}
}

// The prompt commands must expand at the App.Run boundary (the same
// place $name skills expand), so every entry surface — REPL, TUI,
// print mode — gets the workflow for free.
func TestPromptCommandExpandsAtRunBoundary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatSSE(w, "done", 10)
	}))
	defer srv.Close()
	setupTestApp(t, srv)

	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck
	r := &Renderer{Out: io.Discard, Err: io.Discard}
	ctx := context.Background()

	lastUser := func() llm.Message {
		msgs := app.Tr.Messages()
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == llm.RoleUser {
				return msgs[i]
			}
		}
		t.Fatal("no user message in transcript")
		return llm.Message{}
	}

	if err := runPrompt(ctx, app, r, "/commit focus the message on perf"); err != nil {
		t.Fatal(err)
	}
	text := lastUser().Content[0].Text
	if !strings.Contains(text, "Create a git commit for the current changes.") {
		t.Fatalf("/commit did not expand at the run boundary:\n%s", truncateFor(text, 200))
	}
	if !strings.Contains(text, "Additional instructions from the user: focus the message on perf") {
		t.Fatalf("trailing instructions lost:\n%s", truncateFor(text, 200))
	}

	if err := runPrompt(ctx, app, r, "/commit-push-pr"); err != nil {
		t.Fatal(err)
	}
	if text = lastUser().Content[0].Text; !strings.Contains(text, "open a pull request") {
		t.Fatalf("/commit-push-pr did not expand:\n%s", truncateFor(text, 200))
	}

	if err := runPrompt(ctx, app, r, "/init"); err != nil {
		t.Fatal(err)
	}
	if text = lastUser().Content[0].Text; !strings.Contains(text, "Create or update the AGENTS.md file") {
		t.Fatalf("/init did not expand:\n%s", truncateFor(text, 200))
	}

	// Look-alikes pass through verbatim (App.Command's case rule).
	if err := runPrompt(ctx, app, r, "/committing"); err != nil {
		t.Fatal(err)
	}
	if text = lastUser().Content[0].Text; text != "/committing" {
		t.Fatalf("look-alike line rewritten: %q", text)
	}
}
