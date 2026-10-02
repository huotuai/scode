package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"scode/internal/agent"
	"scode/internal/llm"
	"scode/internal/sandbox"
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

func TestReadToolLineNumbers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o644) //nolint:errcheck

	res := (ReadTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{
		"path": "f.txt", "offset": 2,
	}))
	if res.IsError {
		t.Fatal(blockText(res))
	}
	text := blockText(res)
	if !strings.Contains(text, "2 | beta") || !strings.Contains(text, "3 | gamma") {
		t.Fatalf("missing line-number prefixes: %q", text)
	}
	if strings.Contains(text, "1 | alpha") {
		t.Fatalf("offset=2 must not show line 1: %q", text)
	}
}

func TestReadToolMissing(t *testing.T) {
	res := (ReadTool{}).Execute(agent.ToolContext{CWD: t.TempDir()}, mustArgs(t, map[string]any{"path": "nope.txt"}))
	if !res.IsError {
		t.Fatal("missing file must be an error result")
	}
}

// An empty file must not become an image block even when named .png: the
// empty data URL poisons every later provider request (Kimi rejects it as
// "unsupported image format: text/plain; charset=utf-8").
func TestReadToolEmptyImageIsNotAnImageBlock(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "empty.png"), nil, 0o644) //nolint:errcheck
	res := (ReadTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{"path": "empty.png"}))
	if res.IsError {
		t.Fatal(blockText(res))
	}
	for _, b := range res.Content {
		if b.Kind == llm.BlockImage {
			t.Fatalf("empty .png produced an image block: %+v", b)
		}
	}
}

// Detection is by content, not extension: a text file named .png reads as
// text, and real image bytes become an image block regardless of name.
func TestReadToolDetectsImageByContent(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "fake.png"), []byte("just text"), 0o644) //nolint:errcheck
	res := (ReadTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{"path": "fake.png"}))
	if res.IsError {
		t.Fatal(blockText(res))
	}
	for _, b := range res.Content {
		if b.Kind == llm.BlockImage {
			t.Fatal("text content named .png was treated as an image")
		}
	}

	png := append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, 0, 0, 0, 13)
	png = append(png, 'I', 'H', 'D', 'R')
	os.WriteFile(filepath.Join(dir, "real"), png, 0o644) //nolint:errcheck
	res = (ReadTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{"path": "real"}))
	if res.IsError {
		t.Fatal(blockText(res))
	}
	found := false
	for _, b := range res.Content {
		if b.Kind == llm.BlockImage && b.MimeType == "image/png" && b.Data != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("real PNG bytes without an extension were not attached")
	}
}

func TestWriteTool(t *testing.T) {
	dir := t.TempDir()
	res := (WriteTool{}).Execute(unfencedCtx(dir), mustArgs(t, map[string]any{
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
	res := (WriteTool{}).Execute(unfencedCtx(dir), mustArgs(t, map[string]any{
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

func TestFindToolGlobBasenameSemantics(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755)                          //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "root.go"), []byte("x"), 0o644)           //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "a", "mid.go"), []byte("x"), 0o644)       //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "a", "b", "deep.go"), []byte("x"), 0o644) //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "skip.txt"), []byte("x"), 0o644)          //nolint:errcheck

	// Slash-free globs match at ANY depth (rg/fd convention).
	res := (FindTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{"pattern": "*.go"}))
	if res.IsError {
		t.Fatal(blockText(res))
	}
	text := blockText(res)
	for _, want := range []string{"root.go", "a/mid.go", "a/b/deep.go"} {
		if !strings.Contains(text, want) {
			t.Fatalf("pattern *.go missed %q: %q", want, text)
		}
	}
	// Patterns with "/" stay full-path.
	res = (FindTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{"pattern": "a/*.go"}))
	if res.IsError {
		t.Fatal(blockText(res))
	}
	text = blockText(res)
	if !strings.Contains(text, "a/mid.go") || strings.Contains(text, "root.go") || strings.Contains(text, "deep.go") {
		t.Fatalf("anchored pattern wrong: %q", text)
	}
}

func TestGrepToolGlobBasename(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)                                   //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "root.go"), []byte("target\n"), 0o644)          //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "sub", "nested.go"), []byte("target\n"), 0o644) //nolint:errcheck
	res := (GrepTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{
		"pattern": "target", "glob": "*.go",
	}))
	if res.IsError {
		t.Fatal(blockText(res))
	}
	text := blockText(res)
	if !strings.Contains(text, "root.go:1") || !strings.Contains(text, "sub/nested.go:1") {
		t.Fatalf("glob *.go missed nested files: %q", text)
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

func TestGrepToolFilesOnly(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n// needle here\n"), 0o644) //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "b.go"), []byte("package b\n"), 0o644)                 //nolint:errcheck
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)                                          //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "sub", "c.txt"), []byte("needle again\n"), 0o644)      //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "bin.dat"), []byte("needle\x00binary"), 0o644)         //nolint:errcheck

	res := (GrepTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{
		"pattern": "needle", "filesOnly": true,
	}))
	if res.IsError {
		t.Fatal(blockText(res))
	}
	text := blockText(res)
	if !strings.Contains(text, "a.go") || !strings.Contains(text, "sub/c.txt") {
		t.Fatalf("expected matching files: %q", text)
	}
	if strings.Contains(text, "b.go") || strings.Contains(text, "bin.dat") {
		t.Fatalf("non-matching or binary file listed: %q", text)
	}
	if strings.Contains(text, ": ") || strings.Contains(text, "> ") {
		t.Fatalf("filesOnly must not emit match lines: %q", text)
	}

	// glob still applies in filesOnly mode.
	res = (GrepTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{
		"pattern": "needle", "filesOnly": true, "glob": "*.go",
	}))
	text = blockText(res)
	if !strings.Contains(text, "a.go") || strings.Contains(text, "c.txt") {
		t.Fatalf("glob not applied in filesOnly mode: %q", text)
	}
}

func TestCodeMapToolGo(t *testing.T) {
	dir := t.TempDir()
	src := "package demo\n\nimport \"fmt\"\n\ntype Server struct {\n\tport int\n}\n\nfunc NewServer(port int) *Server {\n\treturn &Server{port: port}\n}\n\nfunc (s *Server) Start() error {\n\tfmt.Println(s.port)\n\treturn nil\n}\n\nfunc helper() {}\n"
	os.WriteFile(filepath.Join(dir, "srv.go"), []byte(src), 0o644)         //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "readme.md"), []byte("# hi\n"), 0o644) //nolint:errcheck

	res := (CodeMapTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{"path": "."}))
	if res.IsError {
		t.Fatal(blockText(res))
	}
	text := blockText(res)
	for _, want := range []string{
		"srv.go:",
		"5: type Server struct",
		"9: func NewServer(port int) *Server",
		"13: func (s *Server) Start() error",
		"18: func helper()",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("outline missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "readme.md") {
		t.Fatalf("unsupported extension must be skipped:\n%s", text)
	}
	if strings.Contains(text, "import") {
		t.Fatalf("imports must not appear in the outline:\n%s", text)
	}
}

func TestCodeMapToolTSAndPy(t *testing.T) {
	dir := t.TempDir()
	ts := "import x from 'y';\n\nexport interface Config {\n\ta: number;\n}\n\nexport function run(c: Config): void {}\n\nconst helper = async () => {};\n\nfunction nested() {\n\tfunction inner() {}\n}\n"
	os.WriteFile(filepath.Join(dir, "mod.ts"), []byte(ts), 0o644) //nolint:errcheck
	py := "import os\n\nclass App:\n    def method(self):\n        pass\n\nasync def main():\n    pass\n\ndef _private():\n    pass\n"
	os.WriteFile(filepath.Join(dir, "app.py"), []byte(py), 0o644) //nolint:errcheck

	res := (CodeMapTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{}))
	if res.IsError {
		t.Fatal(blockText(res))
	}
	text := blockText(res)
	for _, want := range []string{
		"mod.ts:", "3: export interface Config", "7: export function run(c: Config): void {}",
		"9: const helper = async () => {};",
		"app.py:", "3: class App:", "7: async def main():", "10: def _private():",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("outline missing %q:\n%s", want, text)
		}
	}
	// Nested declarations stay out (column-0 anchoring).
	if strings.Contains(text, "inner()") || strings.Contains(text, "method(self)") {
		t.Fatalf("nested symbols must not appear:\n%s", text)
	}
}

func TestCodeMapToolSingleFileUnsupported(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x.md"), []byte("# hi\n"), 0o644) //nolint:errcheck
	res := (CodeMapTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{"path": "x.md"}))
	if !res.IsError {
		t.Fatal("unsupported single file must error with guidance")
	}
}

func TestGrepToolBadPattern(t *testing.T) {
	res := (GrepTool{}).Execute(agent.ToolContext{CWD: t.TempDir()}, mustArgs(t, map[string]any{"pattern": "("}))
	if !res.IsError {
		t.Fatal("bad regex must error")
	}
}

func TestBashTool(t *testing.T) {
	res := (BashTool{}).Execute(unfencedCtx(t.TempDir()), mustArgs(t, map[string]any{
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
	res := (BashTool{}).Execute(unfencedCtx(t.TempDir()), mustArgs(t, map[string]any{
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

func TestBashToolTimeoutDetaches(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(cleanupBackgroundTasks)
	res := (BashTool{}).Execute(unfencedCtx(dir), mustArgs(t, map[string]any{
		"command": "sleep 30", "timeout": 0.3,
	}))
	if res.IsError {
		t.Skipf("no bash on this machine: %s", blockText(res))
	}
	text := blockText(res)
	id := taskIDFromText(t, text)
	if !strings.Contains(text, "still running in the background") {
		t.Fatalf("timeout result should announce the detached task: %q", text)
	}
	if info, ok := GetBackgroundTask(id); !ok || info.State != TaskRunning {
		t.Fatalf("detached task must be visible and running: ok=%v info=%+v", ok, info)
	}

	kill := (BashKillTool{}).Execute(unfencedCtx(dir), mustArgs(t, map[string]any{"id": id}))
	if kill.IsError {
		t.Fatalf("bash_kill failed: %s", blockText(kill))
	}
	if info := waitTaskState(t, id); info.State != TaskKilled {
		t.Fatalf("state = %s, want killed", info.State)
	}
}

func TestBashToolRunInBackground(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(cleanupBackgroundTasks)
	start := time.Now()
	res := (BashTool{}).Execute(unfencedCtx(dir), mustArgs(t, map[string]any{
		"command": "echo bg-ready; sleep 30", "run_in_background": true,
	}))
	if res.IsError {
		t.Skipf("no bash on this machine: %s", blockText(res))
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("run_in_background must return immediately, took %v", elapsed)
	}
	id := taskIDFromText(t, blockText(res))

	// The command's early output must show up through bash_status.
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := blockText((BashStatusTool{}).Execute(unfencedCtx(dir), mustArgs(t, map[string]any{"id": id})))
		if strings.Contains(st, "bg-ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background output never appeared: %q", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := KillBackgroundTask(id); err != nil {
		t.Fatalf("kill: %v", err)
	}
}

func TestBashStatusReportsExitCodeAndOutput(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(cleanupBackgroundTasks)
	res := (BashTool{}).Execute(unfencedCtx(dir), mustArgs(t, map[string]any{
		"command": "echo done-marker; exit 7", "run_in_background": true,
	}))
	if res.IsError {
		t.Skipf("no bash on this machine: %s", blockText(res))
	}
	id := taskIDFromText(t, blockText(res))
	if info := waitTaskState(t, id); info.State != TaskExited || info.ExitCode != 7 {
		t.Fatalf("terminal state = %+v, want exited/7", info)
	}
	text := blockText((BashStatusTool{}).Execute(unfencedCtx(dir), mustArgs(t, map[string]any{"id": id})))
	if !strings.Contains(text, "exit code 7") || !strings.Contains(text, "done-marker") {
		t.Fatalf("bash_status lost the result: %q", text)
	}
}

func TestBashStatusListsTasksWithoutID(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(cleanupBackgroundTasks)
	tc := unfencedCtx(dir)
	tc.Env = map[string]string{"SCODE_SESSION_ID": "session-under-test"}
	res := (BashTool{}).Execute(tc, mustArgs(t, map[string]any{
		"command": "echo listed; sleep 30", "run_in_background": true,
	}))
	if res.IsError {
		t.Skipf("no bash on this machine: %s", blockText(res))
	}
	id := taskIDFromText(t, blockText(res))

	listed := blockText((BashStatusTool{}).Execute(tc, mustArgs(t, map[string]any{})))
	if !strings.Contains(listed, fmt.Sprintf("#%d", id)) || !strings.Contains(listed, "listed") {
		t.Fatalf("list lost the task: %q", listed)
	}
	// Session filtering: another session must not see it.
	other := unfencedCtx(dir)
	other.Env = map[string]string{"SCODE_SESSION_ID": "someone-else"}
	if otherList := blockText((BashStatusTool{}).Execute(other, mustArgs(t, map[string]any{}))); strings.Contains(otherList, fmt.Sprintf("#%d", id)) {
		t.Fatalf("another session saw the task: %q", otherList)
	}
	if err := KillBackgroundTask(id); err != nil {
		t.Fatalf("kill: %v", err)
	}
}

// The timeout/exit race: a short command under a tiny timeout may finish in
// time or be detached, but it must never lose its result — the single
// cmd.Wait owner is what makes that structural.
func TestBashToolTimeoutRaceDoesNotLoseResults(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(cleanupBackgroundTasks)
	for i := 0; i < 15; i++ {
		res := (BashTool{}).Execute(unfencedCtx(dir), mustArgs(t, map[string]any{
			"command": "echo raced", "timeout": 0.01,
		}))
		if res.IsError {
			t.Skipf("no bash on this machine: %s", blockText(res))
		}
		text := blockText(res)
		if !strings.Contains(text, "raced") && !strings.Contains(text, "background task #") {
			t.Fatalf("run %d produced neither output nor a task handle: %q", i, text)
		}
		if strings.Contains(text, "background task #") {
			waitTaskState(t, taskIDFromText(t, text))
		}
	}
}

func TestBackgroundRegistryConcurrentCap(t *testing.T) {
	resetBackgroundRegistry()
	defer resetBackgroundRegistry()
	mk := func() *backgroundTask {
		return &backgroundTask{out: newLockedTailBuffer(16), done: make(chan struct{}), startedAt: time.Now()}
	}
	for i := 0; i < maxConcurrentBackgroundTasks; i++ {
		if err := registerBackgroundTask(mk()); err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}
	if err := registerBackgroundTask(mk()); err == nil {
		t.Fatal("the concurrent cap must refuse another live task")
	}
}

func TestBashStatusDurationFreezesOnExit(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(cleanupBackgroundTasks)
	res := (BashTool{}).Execute(unfencedCtx(dir), mustArgs(t, map[string]any{
		"command": "echo done", "run_in_background": true,
	}))
	if res.IsError {
		t.Skipf("no bash on this machine: %s", blockText(res))
	}
	id := taskIDFromText(t, blockText(res))
	first := waitTaskState(t, id)

	// A finished task's clock must stop: polling again later must not grow
	// the duration the UI shows.
	time.Sleep(200 * time.Millisecond)
	second, ok := GetBackgroundTask(id)
	if !ok {
		t.Fatalf("task #%d vanished", id)
	}
	if second.State != first.State || second.DurationMs != first.DurationMs {
		t.Fatalf("duration kept ticking after exit: first=%+v second=%+v", first, second)
	}
}

// taskIDFromText pulls the registry id out of a bash result or status line.
func taskIDFromText(t *testing.T, text string) int {
	t.Helper()
	m := regexp.MustCompile(`background task #(\d+)`).FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no background task id in %q", text)
	}
	id, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("bad task id in %q: %v", text, err)
	}
	return id
}

// waitTaskState blocks until a task is no longer running.
func waitTaskState(t *testing.T, id int) TaskInfo {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		info, ok := GetBackgroundTask(id)
		if !ok {
			t.Fatalf("task #%d vanished from the registry", id)
		}
		if info.State != TaskRunning {
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("task #%d stayed running: %+v", id, info)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// cleanupBackgroundTasks kills live tasks and waits for them to die, so a
// test's TempDir (the command's cwd) is not held open at RemoveAll time.
func cleanupBackgroundTasks() {
	KillAllBackgroundTasks()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		running := false
		for _, info := range ListBackgroundTasks("") {
			if info.State == TaskRunning {
				running = true
				break
			}
		}
		if !running {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func resetBackgroundRegistry() {
	bgRegistry.mu.Lock()
	bgRegistry.tasks = map[int]*backgroundTask{}
	bgRegistry.order = nil
	bgRegistry.next = 0
	bgRegistry.mu.Unlock()
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

func TestGrepToolContextLineFormat(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("head\nMATCH\nfoot\n"), 0o644) //nolint:errcheck
	res := (GrepTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{
		"pattern": "MATCH", "context": 1,
	}))
	if res.IsError {
		t.Fatal(blockText(res))
	}
	text := blockText(res)
	// Match: path:line: / context: path-line- (grep convention).
	if !strings.Contains(text, "f.txt:2: MATCH") {
		t.Fatalf("match line format wrong: %q", text)
	}
	if !strings.Contains(text, "f.txt-1- head") || !strings.Contains(text, "f.txt-3- foot") {
		t.Fatalf("context line format wrong: %q", text)
	}
}

func TestBashToolCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	res := (BashTool{}).Execute(unfencedCtxWithCtx(t.TempDir(), ctx), mustArgs(t, map[string]any{
		"command": "sleep 30",
	}))
	if res.IsError {
		if strings.Contains(blockText(res), "aborted") {
			return // expected
		}
		t.Skipf("no bash: %s", blockText(res))
	}
	t.Fatal("cancel must abort the command")
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

// The sandbox fence on file mutations: workspace-write confines writes to
// the workspace/temp roots; read-only denies all; reads stay unfenced.
func TestSandboxFence(t *testing.T) {
	dir := t.TempDir()
	pol := &agent.SandboxPolicy{Mode: "workspace-write", WorkspaceRoot: dir}

	// Inside the workspace: allowed.
	res := (WriteTool{}).Execute(agent.ToolContext{CWD: dir, Sandbox: pol}, mustArgs(t, map[string]any{
		"path": "ok.txt", "content": "x",
	}))
	if res.IsError {
		t.Fatalf("inside write denied: %v", res.Content)
	}

	// Outside: denied with the model-facing markers.
	res = (WriteTool{}).Execute(agent.ToolContext{CWD: dir, Sandbox: pol}, mustArgs(t, map[string]any{
		"path": filepath.Join(os.TempDir(), "..", "scode-fence-test.txt"), "content": "x",
	}))
	if !res.IsError {
		t.Fatal("outside write should be denied")
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "[sandbox: file access denied under workspace-write mode]") ||
		!strings.Contains(text, "[sandbox: escalation available") {
		t.Fatalf("markers missing: %s", text)
	}

	// read-only denies even inside the workspace.
	pol.Mode = "read-only"
	res = (WriteTool{}).Execute(agent.ToolContext{CWD: dir, Sandbox: pol}, mustArgs(t, map[string]any{
		"path": "nope.txt", "content": "x",
	}))
	if !res.IsError || !strings.Contains(res.Content[0].Text, "read-only") {
		t.Fatalf("read-only: %v", res.Content)
	}

	// Reads are never fenced (dsh: every mode permits reading).
	res = (ReadTool{}).Execute(agent.ToolContext{CWD: dir, Sandbox: pol}, mustArgs(t, map[string]any{
		"path": "ok.txt",
	}))
	if res.IsError {
		t.Fatalf("read fenced: %v", res.Content)
	}

	// bash under a confined mode: on platforms WITH a runner backend
	// (Windows ACL) the command runs confined; elsewhere it fails CLOSED.
	res = (BashTool{}).Execute(agent.ToolContext{CWD: dir, Sandbox: pol}, mustArgs(t, map[string]any{
		"command": "echo hi",
	}))
	if runtime.GOOS == "windows" {
		if res.IsError {
			t.Fatalf("confined bash should run: %v", res.Content)
		}
	} else if !res.IsError || !strings.Contains(res.Content[0].Text, "SANDBOX_UNAVAILABLE") {
		t.Fatalf("bash should fail closed without a backend: %v", res.Content)
	}

	// danger-full-access: unfenced.
	pol.Mode = "danger-full-access"
	res = (BashTool{}).Execute(agent.ToolContext{CWD: dir, Sandbox: pol}, mustArgs(t, map[string]any{
		"command": "echo hi",
	}))
	if res.IsError {
		t.Fatalf("danger mode bash: %v", res.Content)
	}
}

// Sandbox escalation choreography: a confined write that asks for a wider
// mode is judged, approved through the escalation channel, and runs at the
// wider mode for that call only. Rejection and missing pairing fail closed.
func TestSandboxEscalationFlow(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(os.TempDir(), "..", "scode-esc-test.txt")
	defer os.Remove(outside) //nolint:errcheck
	pol := &agent.SandboxPolicy{Mode: "workspace-write", WorkspaceRoot: dir}

	approved := false
	tc := agent.ToolContext{
		CWD:     dir,
		Sandbox: pol,
		Escalate: func(requestedMode, justification, detail string) agent.EscalationResult {
			approved = true
			if requestedMode != "danger-full-access" || justification == "" || detail == "" {
				t.Errorf("escalation request = %q %q %q", requestedMode, justification, detail)
			}
			return agent.EscalationResult{Approved: true}
		},
	}

	// Escalated write outside the boundary: approved → succeeds.
	res := (WriteTool{}).Execute(tc, mustArgs(t, map[string]any{
		"path": outside, "content": "x",
		"sandbox_permissions": "danger-full-access",
		"justification":       "需要写全局日志",
	}))
	if res.IsError || !approved {
		t.Fatalf("escalated write: approved=%v res=%v", approved, res.Content)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("escalated write did not land")
	}
	os.Remove(outside) //nolint:errcheck

	// Same call WITHOUT escalation still denied.
	res = (WriteTool{}).Execute(tc, mustArgs(t, map[string]any{
		"path": outside, "content": "x",
	}))
	if !res.IsError || !strings.Contains(res.Content[0].Text, "[sandbox:") {
		t.Fatalf("unescalated: %v", res.Content)
	}

	// Rejected escalation: nothing runs.
	tc.Escalate = func(string, string, string) agent.EscalationResult {
		return agent.EscalationResult{Reason: "no"}
	}
	res = (WriteTool{}).Execute(tc, mustArgs(t, map[string]any{
		"path": outside, "content": "x",
		"sandbox_permissions": "danger-full-access",
		"justification":       "再试一次",
	}))
	if !res.IsError || !strings.Contains(res.Content[0].Text, "rejected") {
		t.Fatalf("rejected: %v", res.Content)
	}

	// Pairing violation: permissions without justification.
	res = (WriteTool{}).Execute(tc, mustArgs(t, map[string]any{
		"path": outside, "content": "x", "sandbox_permissions": "danger-full-access",
	}))
	if !res.IsError || !strings.Contains(res.Content[0].Text, "justification") {
		t.Fatalf("pairing: %v", res.Content)
	}

	// Narrowing request errors without asking.
	asked := false
	tc.Escalate = func(string, string, string) agent.EscalationResult {
		asked = true
		return agent.EscalationResult{Approved: true}
	}
	res = (WriteTool{}).Execute(tc, mustArgs(t, map[string]any{
		"path": outside, "content": "x",
		"sandbox_permissions": "read-only", "justification": "收窄",
	}))
	if !res.IsError || asked {
		t.Fatalf("narrowing: asked=%v res=%v", asked, res.Content)
	}
}

// A directory link inside the workspace (unix symlink, Windows junction)
// must not let the file tools write outside it. The fence resolves the
// deepest existing ancestor, so the linked ancestor is seen as itself. The
// direct write to the same outside location is asserted first: it proves the
// decoy is genuinely outside every writable root.
func TestWriteRejectsLinkedAncestorEscape(t *testing.T) {
	ws := t.TempDir()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory for the decoy: %v", err)
	}
	outside := filepath.Join(home, fmt.Sprintf("scode-tools-link-%d", os.Getpid()))
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Skipf("cannot create the decoy: %v", err)
	}
	defer os.RemoveAll(outside) //nolint:errcheck

	pol := &agent.SandboxPolicy{Mode: "workspace-write", WorkspaceRoot: ws}
	tc := agent.ToolContext{CWD: ws, Sandbox: pol}

	// Precondition: the decoy is outside the boundary.
	res := (WriteTool{}).Execute(tc, mustArgs(t, map[string]any{
		"path": filepath.Join(outside, "direct.txt"), "content": "x",
	}))
	if !res.IsError {
		t.Skipf("decoy %s is writable, so it is inside a writable root", outside)
	}

	link := filepath.Join(ws, "link")
	if err := os.Symlink(outside, link); err != nil {
		// Windows: a junction needs no privilege and is the realistic
		// vector (and the one Go's EvalSymlinks cannot see).
		if out, jerr := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); jerr != nil {
			t.Skipf("no directory-link primitive: symlink=%v junction=%v %s", err, jerr, out)
		}
	}
	defer os.Remove(link) //nolint:errcheck // remove the link, never its target

	for _, name := range []string{"evil.txt", filepath.Join("sub", "evil.txt")} {
		res = (WriteTool{}).Execute(tc, mustArgs(t, map[string]any{
			"path": filepath.Join(link, name), "content": "pwned",
		}))
		if !res.IsError {
			t.Fatalf("linked-ancestor escape allowed for %s: %v", name, res.Content)
		}
		if !strings.Contains(res.Content[0].Text, "[sandbox:") {
			t.Fatalf("expected the denial marker for %s, got: %s", name, res.Content[0].Text)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); err == nil {
		t.Fatal("a file landed outside the workspace")
	}
	if _, err := os.Stat(filepath.Join(outside, "sub", "evil.txt")); err == nil {
		t.Fatal("a nested file landed outside the workspace")
	}
}

// unfencedCtx is the explicit unrestricted policy the mutation-tool tests
// opt into: a ToolContext with no composed policy is now fail-closed, so
// tests that exercise unrelated behavior must state the mode they assume.
func unfencedCtx(dir string) agent.ToolContext {
	return agent.ToolContext{
		CWD:     dir,
		Sandbox: &agent.SandboxPolicy{Mode: string(sandbox.ModeDangerFullAccess), WorkspaceRoot: dir},
	}
}

func unfencedCtxWithCtx(dir string, ctx context.Context) agent.ToolContext {
	tc := unfencedCtx(dir)
	tc.Ctx = ctx
	return tc
}

// Fail-closed contract: a mutation tool with no composed sandbox policy, or
// with an unresolved mode string, must refuse and leave the filesystem
// untouched — absence of a policy is never permission.
func TestMutationToolsFailClosedWithoutPolicy(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.txt")

	cases := []struct {
		name string
		tc   agent.ToolContext
	}{
		{"no policy", agent.ToolContext{CWD: dir}},
		{"empty mode", agent.ToolContext{CWD: dir, Sandbox: &agent.SandboxPolicy{WorkspaceRoot: dir}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := (WriteTool{}).Execute(c.tc, mustArgs(t, map[string]any{"path": target, "content": "x"}))
			if !res.IsError || !strings.Contains(res.Content[0].Text, "fail-closed") {
				t.Fatalf("write must fail closed: %v", res.Content)
			}
			res = (EditTool{}).Execute(c.tc, mustArgs(t, map[string]any{
				"path": target, "edits": []map[string]string{{"oldText": "a", "newText": "b"}},
			}))
			if !res.IsError || !strings.Contains(res.Content[0].Text, "fail-closed") {
				t.Fatalf("edit must fail closed: %v", res.Content)
			}
			res = (BashTool{}).Execute(c.tc, mustArgs(t, map[string]any{"command": "echo hi"}))
			if !res.IsError || !strings.Contains(res.Content[0].Text, "fail-closed") {
				t.Fatalf("bash must fail closed: %v", res.Content)
			}
			if _, err := os.Stat(target); err == nil {
				t.Fatal("a refused call touched the filesystem")
			}
		})
	}
}

// The approval prompt's detail must be an absolute path (so the human can
// verify what is being widened) and a bounded single line (so a model cannot
// forge layout or bury the payload). bash passes the raw command; write/edit
// must absolutize it before the summarizer sees it.
func TestEscalationDetailIsAbsoluteAndBounded(t *testing.T) {
	dir := t.TempDir()
	pol := &agent.SandboxPolicy{Mode: "workspace-write", WorkspaceRoot: dir}

	var got []string
	tc := agent.ToolContext{
		CWD:     dir,
		Sandbox: pol,
		Escalate: func(requestedMode, justification, detail string) agent.EscalationResult {
			got = append(got, detail)
			return agent.EscalationResult{Approved: false, Reason: "test"}
		},
	}

	// Relative path must arrive absolute.
	(WriteTool{}).Execute(tc, mustArgs(t, map[string]any{
		"path": "sub/rel.txt", "content": "x",
		"sandbox_permissions": "danger-full-access", "justification": "需要写外部目录",
	}))
	if len(got) != 1 {
		t.Fatalf("escalations = %d", len(got))
	}
	if !filepath.IsAbs(got[0]) {
		t.Errorf("write detail is not absolute: %q", got[0])
	}
	if !strings.HasSuffix(got[0], filepath.Join("sub", "rel.txt")) {
		t.Errorf("write detail lost the target: %q", got[0])
	}

	// A long multi-line command must arrive as one bounded line that still
	// shows its tail, and no newline may survive.
	got = nil
	long := "echo start\n" + strings.Repeat("padding ", 60) + "&& echo DANGEROUS-TAIL"
	(BashTool{}).Execute(tc, mustArgs(t, map[string]any{
		"command":             long,
		"sandbox_permissions": "danger-full-access", "justification": "需要更宽的文件范围",
	}))
	if len(got) != 1 {
		t.Fatalf("escalations = %d", len(got))
	}
	if strings.ContainsAny(got[0], "\n\r") {
		t.Errorf("command detail kept newlines: %q", got[0])
	}
	if !strings.Contains(got[0], "DANGEROUS-TAIL") {
		t.Errorf("command tail hidden from the approver: %q", got[0])
	}
	if n := len([]rune(got[0])); n > 300 {
		t.Errorf("command detail unbounded: %d runes", n)
	}
}

// The justification is model-controlled text rendered verbatim in the
// approval card, so it gets the same bounded, layout-safe treatment.
func TestEscalationJustificationIsBounded(t *testing.T) {
	dir := t.TempDir()
	var got string
	tc := agent.ToolContext{
		CWD:     dir,
		Sandbox: &agent.SandboxPolicy{Mode: "workspace-write", WorkspaceRoot: dir},
		Escalate: func(_ string, justification, _ string) agent.EscalationResult {
			got = justification
			return agent.EscalationResult{Approved: false, Reason: "test"}
		},
	}
	(WriteTool{}).Execute(tc, mustArgs(t, map[string]any{
		"path": filepath.Join(dir, "a.txt"), "content": "x",
		"sandbox_permissions": "danger-full-access",
		"justification":       "need it\n[y] allow once\n" + strings.Repeat("pad ", 200),
	}))
	if got == "" {
		t.Fatal("no escalation was raised")
	}
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("justification kept newlines: %q", got)
	}
	if n := len([]rune(got)); n > 300 {
		t.Errorf("justification unbounded: %d runes", n)
	}
}

// The escalation vocabulary is advertised in three tool schemas, while the
// strictly-wider ladder lives in the sandbox package. Nothing in the type
// system ties them together, so this test does: an enum that drifts would
// either advertise a rung the judge rejects, or hide one the model needs to
// name (stranding a session switched below the deployment default).
func TestEscalationVocabularyMatchesSchemas(t *testing.T) {
	advertised := map[string]bool{}
	for _, m := range sandbox.EscalationTargets {
		if !m.Valid() {
			t.Fatalf("EscalationTargets contains an invalid mode: %q", m)
		}
		advertised[string(m)] = true
	}
	ladder := map[string]bool{}
	for _, wider := range sandbox.WiderModes {
		for _, m := range wider {
			ladder[string(m)] = true
		}
	}
	if len(advertised) != len(ladder) {
		t.Fatalf("advertised %v != ladder %v", advertised, ladder)
	}
	for m := range ladder {
		if !advertised[m] {
			t.Errorf("ladder mode %q is not advertised to the model", m)
		}
	}

	tools := map[string]agent.Tool{
		"bash":  BashTool{},
		"write": WriteTool{},
		"edit":  EditTool{},
	}
	for name, tool := range tools {
		var schema struct {
			Properties map[string]struct {
				Type string   `json:"type"`
				Enum []string `json:"enum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(tool.Decl().Parameters, &schema); err != nil {
			t.Fatalf("%s schema is not valid JSON: %v", name, err)
		}
		perm, ok := schema.Properties["sandbox_permissions"]
		if !ok {
			t.Fatalf("%s schema does not advertise sandbox_permissions", name)
		}
		if perm.Type != "string" {
			t.Errorf("%s sandbox_permissions type = %q", name, perm.Type)
		}
		if len(perm.Enum) != len(advertised) {
			t.Errorf("%s enum = %v, want %v", name, perm.Enum, sandbox.EscalationTargets)
		}
		for _, e := range perm.Enum {
			if !advertised[e] {
				t.Errorf("%s advertises %q, which is not an escalation target", name, e)
			}
		}
		if j, ok := schema.Properties["justification"]; !ok || j.Type != "string" {
			t.Errorf("%s must advertise the paired justification field", name)
		}
	}
}
