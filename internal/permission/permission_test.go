package permission

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"scode/internal/llm"
)

func call(name string, args map[string]any) llm.Block {
	raw, _ := json.Marshal(args)
	return llm.Block{Kind: llm.BlockToolCall, Name: name, Arguments: raw}
}

func bashCmd(cmd string) llm.Block { return call("bash", map[string]any{"command": cmd}) }
func writeTo(p string) llm.Block   { return call("write", map[string]any{"path": p, "content": "x"}) }

func mustEngine(t *testing.T, cwd, mode string, allow, ask, deny []string) *Engine {
	t.Helper()
	e, err := New(cwd, mode, allow, ask, deny)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func TestParseRule(t *testing.T) {
	r, err := ParseRule("bash(git status:*)")
	if err != nil {
		t.Fatal(err)
	}
	if r.Tool != "bash" || r.Pattern != "git status:*" || !r.HasPattern {
		t.Fatalf("got %+v", r)
	}
	r, err = ParseRule("read")
	if err != nil || r.HasPattern || r.Tool != "read" {
		t.Fatalf("name-only: %+v %v", r, err)
	}
	if _, err := ParseRule(""); err == nil {
		t.Fatal("empty rule accepted")
	}
	if _, err := ParseRule("bash(git status"); err == nil {
		t.Fatal("unbalanced paren accepted")
	}
	if _, err := ParseRule("(x)"); err == nil {
		t.Fatal("empty tool name accepted")
	}
}

func TestBashMatching(t *testing.T) {
	e := mustEngine(t, "", "", nil, nil, []string{`bash(git push:*)`, `bash(rm -rf /)`})
	cases := []struct {
		cmd  string
		want Decision
	}{
		{"git push origin main", Deny},
		{"git push", Deny},
		{"git pushx", Allow}, // :* continuation needs a word boundary
		{"git status", Allow},
		{"rm -rf /", Deny},
		{"rm -rf /tmp/x", Allow}, // plain pattern: word-boundary prefix
	}
	for _, c := range cases {
		if got, _ := e.Evaluate(bashCmd(c.cmd)); got != c.want {
			t.Errorf("%q: got %v want %v", c.cmd, got, c.want)
		}
	}
}

func TestBashChainMatching(t *testing.T) {
	// Deny/ask tiers fire on ANY matching segment; the allow tier must
	// cover EVERY segment — an allow for "npm test" never green-lights
	// "npm test && anything-else".
	e := mustEngine(t, "", "",
		[]string{`bash(npm test)`},
		[]string{`bash(rm:*)`},
		[]string{`bash(git push:*)`},
	)
	cases := []struct {
		cmd      string
		want     Decision
		wantRule string // "" = decided by the mode default, no rule matched
	}{
		{"npm test", Allow, "bash(npm test)"},
		{"npm test && npm test", Allow, "bash(npm test)"},
		{"npm test;", Allow, "bash(npm test)"},                      // trailing separator
		{"npm test && rm -rf x", Ask, "bash(rm:*)"},                 // allow must not green-light the chain
		{"npm test; rm x", Ask, "bash(rm:*)"},                       // ; segment
		{"npm test\nrm x", Ask, "bash(rm:*)"},                       // newline segment
		{"npm test && git status", Allow, ""},                       // allow rule no longer covers the chain
		{"git status && git push origin", Deny, "bash(git push:*)"}, // deny fires on any segment
		{"echo hi; git push", Deny, "bash(git push:*)"},             // bare prefix denied mid-chain
		{"git diff HEAD", Allow, ""},                                // unmatched: mode default
	}
	for _, c := range cases {
		got, r := e.Evaluate(bashCmd(c.cmd))
		gotRule := ""
		if r != nil {
			gotRule = r.Raw
		}
		if got != c.want || gotRule != c.wantRule {
			t.Errorf("%q: got %v %q, want %v %q", c.cmd, got, gotRule, c.want, c.wantRule)
		}
	}
}

func TestDenyAskAllowPrecedence(t *testing.T) {
	e := mustEngine(t, "", "",
		[]string{`bash(git:*)`},        // allow git anything
		[]string{`bash(git commit:*)`}, // but commits ask
		[]string{`bash(git push:*)`},   // and pushes deny outright
	)
	if d, r := e.Evaluate(bashCmd("git push origin")); d != Deny || r.Raw != `bash(git push:*)` {
		t.Errorf("push: %v %v", d, r)
	}
	if d, _ := e.Evaluate(bashCmd("git commit -m x")); d != Ask {
		t.Errorf("commit: %v", d)
	}
	if d, _ := e.Evaluate(bashCmd("git status")); d != Allow {
		t.Errorf("status: %v", d)
	}
}

func TestPathMatching(t *testing.T) {
	dir := t.TempDir()
	// List-level precedence is absolute (deny > ask > allow regardless of
	// specificity): a whitelist carve-out of a broad deny is NOT
	// expressible here — plan mode's .scode/plan exemption lives in the
	// mode's own evaluation branch (P0'b), not in these lists.
	e := mustEngine(t, dir, "",
		[]string{`read(**)`},
		[]string{`write(src/**)`},
		[]string{`write(.git/**)`},
	)
	if d, r := e.Evaluate(writeTo(".git/config")); d != Deny || r.Raw != `write(.git/**)` {
		t.Errorf(".git write: %v (%v)", d, r)
	}
	if d, _ := e.Evaluate(writeTo("src/main.go")); d != Ask {
		t.Errorf("src write: %v", d)
	}
	// Absolute paths inside cwd relativize before matching.
	if d, _ := e.Evaluate(writeTo(filepath.Join(dir, ".git", "HEAD"))); d != Deny {
		t.Errorf("absolute .git write: %v", d)
	}
	if d, _ := e.Evaluate(call("read", map[string]any{"path": "anywhere/x.md"})); d != Allow {
		t.Errorf("read: %v", d)
	}
	// Unmatched file tool calls fall through to the mode default.
	if d, _ := e.Evaluate(writeTo("other.txt")); d != Allow {
		t.Errorf("unmatched write: %v", d)
	}
}

func TestToolNameGlob(t *testing.T) {
	e := mustEngine(t, "", "", nil, []string{"mcp__github__*"}, nil)
	if d, _ := e.Evaluate(call("mcp__github__delete_repo", nil)); d != Ask {
		t.Errorf("mcp tool should ask: %v", d)
	}
	if d, _ := e.Evaluate(call("mcp__gitlab__list", nil)); d != Allow {
		t.Errorf("other server should fall through: %v", d)
	}
}

func TestNameOnlyRuleIgnoresPatternlessTools(t *testing.T) {
	// A pattern rule on a tool without matchable args never fires.
	e := mustEngine(t, "", "", nil, []string{`exit_plan_mode(*)`}, nil)
	if d, _ := e.Evaluate(call("exit_plan_mode", map[string]any{"plan": "x"})); d != Allow {
		t.Errorf("pattern on arg-less tool must not match: %v", d)
	}
	e2 := mustEngine(t, "", "", nil, []string{`exit_plan_mode`}, nil)
	if d, _ := e2.Evaluate(call("exit_plan_mode", nil)); d != Ask {
		t.Errorf("name-only rule should match: %v", d)
	}
}

func TestSessionAllow(t *testing.T) {
	e := mustEngine(t, "", "", nil, []string{`bash(*)`}, nil)
	c := bashCmd("terraform apply")
	if d, _ := e.Evaluate(c); d != Ask {
		t.Fatalf("before grant: %v", d)
	}
	e.AllowForSession(ExactRule("", c))
	if d, _ := e.Evaluate(c); d != Allow {
		t.Fatalf("after grant: %v", d)
	}
	if d, _ := e.Evaluate(bashCmd("terraform destroy")); d != Ask {
		t.Fatalf("other commands still ask: %v", d)
	}
}

func TestBypassMode(t *testing.T) {
	e := mustEngine(t, "", "bypass", nil, nil, []string{`bash(*)`})
	if d, _ := e.Evaluate(bashCmd("rm -rf /")); d != Allow {
		t.Fatalf("bypass should allow everything: %v", d)
	}
}

func TestDefaultsTier(t *testing.T) {
	e := mustEngine(t, "", "", []string{"mcp__github__list"}, nil, nil)
	if err := e.AddDefault("mcp__github__*", Ask); err != nil {
		t.Fatal(err)
	}
	// Server default fires for unlisted tools…
	if d, r := e.Evaluate(call("mcp__github__delete_repo", nil)); d != Ask || r.Raw != "mcp__github__*" {
		t.Errorf("default tier: %v (%v)", d, r)
	}
	// …but an explicit settings rule beats the server default across lists.
	if d, _ := e.Evaluate(call("mcp__github__list", nil)); d != Allow {
		t.Errorf("explicit allow should beat server default ask: %v", d)
	}
	// Non-MCP tools are untouched.
	if d, _ := e.Evaluate(bashCmd("ls")); d != Allow {
		t.Errorf("unrelated tool: %v", d)
	}
}

func TestExactRule(t *testing.T) {
	if got := ExactRule("", bashCmd("git status")); got != "bash(git status)" {
		t.Errorf("bash exact: %q", got)
	}
	dir := t.TempDir()
	inside := filepath.Join(dir, "src", "main.go")
	if got := ExactRule(dir, writeTo(inside)); got != "write(src/main.go)" {
		t.Errorf("path exact should relativize: %q", got)
	}
	if got := ExactRule("", call("mcp__github__list", nil)); got != "mcp__github__list" {
		t.Errorf("mcp exact: %q", got)
	}
}

// RemoveDefault undoes AddDefault: the fallback verdict disappears,
// and removal is idempotent (a runtime re-add cannot pile up
// duplicates).
func TestRemoveDefault(t *testing.T) {
	e, err := New("", "", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	mcpCall := call("mcp__x__y", nil)
	if dec, _ := e.Evaluate(mcpCall); dec != Allow {
		t.Fatalf("baseline verdict = %v, want Allow (ModeDefault is unrestricted)", dec)
	}
	if err := e.AddDefault("mcp__x__*", Deny); err != nil {
		t.Fatal(err)
	}
	if dec, _ := e.Evaluate(mcpCall); dec != Deny {
		t.Fatalf("default verdict = %v, want Deny", dec)
	}
	e.RemoveDefault("mcp__x__*")
	if dec, _ := e.Evaluate(mcpCall); dec != Allow {
		t.Fatalf("verdict after remove = %v, want Allow", dec)
	}
	e.RemoveDefault("mcp__x__*") // idempotent
	e.RemoveDefault("mcp__other__*")
}
