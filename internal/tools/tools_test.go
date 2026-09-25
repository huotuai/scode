package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scode/internal/agent"
)

func TestTruncateHeadAndTail(t *testing.T) {
	small := "one\ntwo\n"
	if r := TruncateHead(small); r.Truncated || r.Text != small {
		t.Fatalf("small input truncated: %+v", r)
	}

	big := strings.Repeat("line\n", MaxLines+100)
	r := TruncateHead(big)
	if !r.Truncated || r.Notice == "" {
		t.Fatalf("expected truncation notice: %+v", r)
	}
	lines := strings.Count(strings.TrimRight(r.Text, "\n"), "\n") + 1
	if lines > MaxLines {
		t.Fatalf("kept %d lines > %d", lines, MaxLines)
	}

	// Tail keeps the END of long output.
	tail := TruncateTail(big)
	if !tail.Truncated {
		t.Fatal("expected tail truncation")
	}
	if !strings.Contains(tail.Text, fmt.Sprintf("line %d", MaxLines+100)) && !strings.HasSuffix(strings.TrimRight(tail.Text, "\n"), "line") {
		t.Fatalf("tail lost the end: %q...", tail.Text[:40])
	}
	if !strings.Contains(tail.Text, fmt.Sprintf("\nline\n")) {
		t.Fatalf("tail content wrong: %q", tail.Text[:min(60, len(tail.Text))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestReadToolPaging(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	os.WriteFile(path, []byte("l1\nl2\nl3\nl4\nl5\n"), 0o644) //nolint:errcheck

	res := (ReadTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{
		"path": "f.txt", "offset": 2, "limit": 2,
	}))
	if res.IsError {
		t.Fatal(blockText(res))
	}
	text := blockText(res)
	if !strings.Contains(text, "l2") || !strings.Contains(text, "l3") || strings.Contains(text, "l1\n") {
		t.Fatalf("window = %q", text)
	}
	if !strings.Contains(text, "[Showing lines 2-3 of 5. Use offset=4 to continue.]") {
		t.Fatalf("continuation pointer missing: %q", text)
	}
}

func TestReadToolMissing(t *testing.T) {
	res := (ReadTool{}).Execute(agent.ToolContext{CWD: t.TempDir()}, mustArgs(t, map[string]any{"path": "nope.txt"}))
	if !res.IsError {
		t.Fatal("missing file must be an error result")
	}
}

func TestWriteTool(t *testing.T) {
	dir := t.TempDir()
	res := (WriteTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{
		"path": "sub/dir/f.txt", "content": "hello",
	}))
	if res.IsError {
		t.Fatal(blockText(res))
	}
	data, err := os.ReadFile(filepath.Join(dir, "sub", "dir", "f.txt"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("data = %q err = %v", data, err)
	}
}

func TestWriteToolRefusesDirectory(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "d"), 0o755) //nolint:errcheck
	res := (WriteTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{
		"path": "d", "content": "x",
	}))
	if !res.IsError {
		t.Fatal("writing a directory must fail")
	}
}

func TestLsTool(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)                      //nolint:errcheck
	os.WriteFile(filepath.Join(dir, ".hidden"), []byte("x"), 0o644) //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x"), 0o644)   //nolint:errcheck
	res := (LsTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{}))
	if res.IsError {
		t.Fatal(blockText(res))
	}
	text := blockText(res)
	for _, want := range []string{".hidden", "b.txt", "sub/"} {
		if !strings.Contains(text, want) {
			t.Fatalf("ls missing %s: %q", want, text)
		}
	}
}

func TestFindToolGlobAndGitignore(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src", "inner"), 0o755)                              //nolint:errcheck
	os.MkdirAll(filepath.Join(dir, "node_modules", "pkg"), 0o755)                       //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "src", "a.go"), []byte("x"), 0o644)                 //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "src", "inner", "b.go"), []byte("x"), 0o644)        //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "node_modules", "pkg", "c.go"), []byte("x"), 0o644) //nolint:errcheck
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("gen/\n"), 0o644)             //nolint:errcheck
	os.MkdirAll(filepath.Join(dir, "gen"), 0o755)                                       //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "gen", "d.go"), []byte("x"), 0o644)                 //nolint:errcheck

	res := (FindTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{"pattern": "**/*.go"}))
	if res.IsError {
		t.Fatal(blockText(res))
	}
	text := blockText(res)
	if !strings.Contains(text, "src/a.go") || !strings.Contains(text, "src/inner/b.go") {
		t.Fatalf("find missed matches: %q", text)
	}
	if strings.Contains(text, "node_modules") {
		t.Fatalf("junk dir not skipped: %q", text)
	}
	if strings.Contains(text, "gen/") {
		t.Fatalf("gitignored dir not skipped: %q", text)
	}
}

func TestGrepTool(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)                                          //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello world\nsecond line\n"), 0o644) //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("say hello again\n"), 0o644)   //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "skip.log"), []byte("hello\n"), 0o644)                 //nolint:errcheck
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\n"), 0o644)               //nolint:errcheck

	res := (GrepTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{
		"pattern": "hello", "context": 1,
	}))
	if res.IsError {
		t.Fatal(blockText(res))
	}
	text := blockText(res)
	if !strings.Contains(text, "a.txt:1: hello world") {
		t.Fatalf("match missing: %q", text)
	}
	if !strings.Contains(text, "sub/b.txt:1: say hello again") {
		t.Fatalf("nested match missing: %q", text)
	}
	if strings.Contains(text, "skip.log") {
		t.Fatalf("gitignored file searched: %q", text)
	}
	if !strings.Contains(text, "second line") {
		t.Fatalf("context line missing: %q", text)
	}
}

func TestGrepToolLiteralAndCase(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a.b*c\nA.B*C\n"), 0o644) //nolint:errcheck

	res := (GrepTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{
		"pattern": "a.b*c", "literal": true, "ignoreCase": true,
	}))
	if res.IsError {
		t.Fatal(blockText(res))
	}
	text := blockText(res)
	if strings.Count(text, "f.txt:") != 2 {
		t.Fatalf("literal ignore-case matches = %q", text)
	}
}

func TestGrepToolBadPattern(t *testing.T) {
	res := (GrepTool{}).Execute(agent.ToolContext{CWD: t.TempDir()}, mustArgs(t, map[string]any{"pattern": "("}))
	if !res.IsError {
		t.Fatal("bad regex must error")
	}
}

func TestBashTool(t *testing.T) {
	res := (BashTool{}).Execute(agent.ToolContext{CWD: t.TempDir()}, mustArgs(t, map[string]any{
		"command": "echo hello-bash",
	}))
	if res.IsError {
		t.Skipf("no bash on this machine: %s", blockText(res))
	}
	if text := blockText(res); !strings.Contains(text, "hello-bash") {
		t.Fatalf("output = %q", text)
	}
}

func TestBashToolNonZeroExit(t *testing.T) {
	res := (BashTool{}).Execute(agent.ToolContext{CWD: t.TempDir()}, mustArgs(t, map[string]any{
		"command": "echo out; echo err >&2; exit 3",
	}))
	if res.IsError {
		t.Skipf("no bash on this machine: %s", blockText(res))
	}
	text := blockText(res)
	if !strings.Contains(text, "Command exited with code 3") || !strings.Contains(text, "err") {
		t.Fatalf("output = %q", text)
	}
}

func TestBashToolTimeout(t *testing.T) {
	res := (BashTool{}).Execute(agent.ToolContext{CWD: t.TempDir()}, mustArgs(t, map[string]any{
		"command": "sleep 5", "timeout": 1,
	}))
	if res.IsError {
		if strings.Contains(blockText(res), "timed out") {
			return // expected
		}
		t.Skipf("no bash on this machine: %s", blockText(res))
	}
	t.Fatal("timeout must produce an error result")
}

func TestBashToolEnv(t *testing.T) {
	res := (BashTool{}).Execute(agent.ToolContext{
		CWD: t.TempDir(),
		Env: map[string]string{"SCODE_TEST_VAR": "42"},
	}, mustArgs(t, map[string]any{"command": "echo $SCODE_TEST_VAR"}))
	if res.IsError {
		t.Skipf("no bash: %s", blockText(res))
	}
	if text := blockText(res); !strings.Contains(text, "42") {
		t.Fatalf("env not passed: %q", text)
	}
}

func TestIgnoreMatcher(t *testing.T) {
	m := LoadIgnoreMatcher(t.TempDir()) // no .gitignore
	if m.Match("anything") {
		t.Fatal("empty matcher must not match")
	}

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\n!keep.log\nbuild/\n/root-only.txt\n"), 0o644) //nolint:errcheck
	m = LoadIgnoreMatcher(dir)
	cases := []struct {
		path string
		want bool
	}{
		{"a.log", true},
		{"deep/nested/x.log", true},
		{"keep.log", false},
		{"build/output.js", true},
		{"src/build.ts", false}, // build/ is dir-scoped; plain file named build.ts stays
		{"root-only.txt", true},
		{"sub/root-only.txt", false}, // anchored
	}
	for _, c := range cases {
		if got := m.Match(c.path); got != c.want {
			t.Errorf("Match(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestResolve(t *testing.T) {
	if p := Resolve(agent.ToolContext{CWD: "/base"}, "sub/f.txt"); p != filepath.Join("/base", "sub", "f.txt") {
		t.Fatalf("relative resolve = %q", p)
	}
	abs := filepath.Join(t.TempDir(), "f.txt")
	if p := Resolve(agent.ToolContext{CWD: t.TempDir()}, abs); p != abs {
		t.Fatalf("absolute resolve = %q, want %q", p, abs)
	}
}
