package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scode/internal/agent"
)

func edit(oldText, newText string) editRequest {
	return editRequest{oldText: oldText, newText: newText}
}

func TestApplyEditsExact(t *testing.T) {
	content := "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n"
	out, err := applyEdits(content, []editRequest{edit(`println("hi")`, `println("hello")`)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `println("hello")`) || strings.Contains(out, `println("hi")`) {
		t.Fatalf("out = %q", out)
	}
}

func TestApplyEditsMultipleAgainstOriginal(t *testing.T) {
	content := "a\nb\nc\n"
	// Each edit matches the ORIGINAL content, not the output of the other.
	out, err := applyEdits(content, []editRequest{edit("a", "x"), edit("b", "y")})
	if err != nil {
		t.Fatal(err)
	}
	if out != "x\ny\nc\n" {
		t.Fatalf("out = %q", out)
	}
}

func TestApplyEditsAmbiguous(t *testing.T) {
	_, err := applyEdits("dup\ndup\n", []editRequest{edit("dup", "x")})
	if err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("err = %v", err)
	}
}

func TestApplyEditsNotFound(t *testing.T) {
	_, err := applyEdits("abc\n", []editRequest{edit("xyz", "x")})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestApplyEditsOverlapRejected(t *testing.T) {
	_, err := applyEdits("abcdef\n", []editRequest{edit("bcd", "x"), edit("cde", "y")})
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("err = %v", err)
	}
}

func TestApplyEditsNoOp(t *testing.T) {
	_, err := applyEdits("same\n", []editRequest{edit("same", "same")})
	if err == nil || !strings.Contains(err.Error(), "no changes") {
		t.Fatalf("err = %v", err)
	}
}

func TestApplyEditsEmptyOldText(t *testing.T) {
	_, err := applyEdits("x\n", []editRequest{edit("", "y")})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v", err)
	}
}

func TestApplyEditsSmartQuotesFuzzy(t *testing.T) {
	content := "msg := “hello”\n"
	out, err := applyEdits(content, []editRequest{edit(`msg := "hello"`, `msg := "world"`)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"world"`) {
		t.Fatalf("fuzzy quote match failed: %q", out)
	}
}

func TestApplyEditsUnicodeDashesFuzzy(t *testing.T) {
	content := "flag –– enabled\n"
	out, err := applyEdits(content, []editRequest{edit("flag -- enabled", "flag -- disabled")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "disabled") {
		t.Fatalf("fuzzy dash match failed: %q", out)
	}
}

func TestApplyEditsFullwidthFuzzy(t *testing.T) {
	// NFKC folds fullwidth ASCII to ASCII — common with CJK input.
	content := "fmt.Println（“你好”）\n"
	out, err := applyEdits(content, []editRequest{edit(`fmt.Println("你好")`, `fmt.Println("世界")`)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "世界") {
		t.Fatalf("fullwidth fuzzy match failed: %q", out)
	}
}

func TestApplyEditsTrailingWhitespaceFuzzy(t *testing.T) {
	content := "line one   \nline two\n"
	out, err := applyEdits(content, []editRequest{edit("line one\nline two", "LINE ONE\nline two")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "LINE ONE") {
		t.Fatalf("trailing-ws fuzzy match failed: %q", out)
	}
}

func TestApplyEditsMultiLineFuzzyPreservesUnmatched(t *testing.T) {
	content := "keep1\n\told line   \nkeep2\n"
	out, err := applyEdits(content, []editRequest{edit("old line\nkeep2", "new line\nkeep2")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "keep1") || !strings.Contains(out, "new line") || !strings.Contains(out, "keep2") {
		t.Fatalf("out = %q", out)
	}
}

// The exact bug the audit found: an earlier smart quote on the same line
// shifts normalized offsets — the old byte back-mapping spliced the
// wrong position. Line-widened replacement must keep the prefix intact
// (in normalized form) instead of corrupting adjacent bytes.
func TestApplyEditsSmartQuoteEarlierInLine(t *testing.T) {
	// oldText uses ASCII quotes where the file has smart quotes, so the
	// match necessarily goes through the fuzzy path; the earlier smart
	// quote in “start” is what used to shift the offsets.
	content := "label = “start” value = “x” end   \nnext = 2\n"
	out, err := applyEdits(content, []editRequest{edit(`value = "x" end`, `value = "y" end`)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `value = "y" end`) || !strings.Contains(out, "next = 2") {
		t.Fatalf("out = %q", out)
	}
	// The untouched prefix survives (normalized: smart quotes folded).
	if !strings.HasPrefix(out, `label = "start" value = "y" end`) {
		t.Fatalf("prefix corrupted: %q", out)
	}
	if !strings.HasSuffix(out, "next = 2\n") {
		t.Fatalf("suffix corrupted: %q", out)
	}
}

// Fuzzy match spanning multiple lines replaces the touched lines while
// lines outside the window stay byte-exact.
func TestApplyEditsFuzzyMultiLineWidth(t *testing.T) {
	content := "keep0\nold a   \nold b\nkeep3\n"
	out, err := applyEdits(content, []editRequest{edit("old a\nold b", "new a\nnew b")})
	if err != nil {
		t.Fatal(err)
	}
	if out != "keep0\nnew a\nnew b\nkeep3\n" {
		t.Fatalf("out = %q", out)
	}
}

func TestDetectAndRestoreLineEndings(t *testing.T) {
	crlf := "a\r\nb\r\n"
	le := detectLineEnding(crlf)
	if le != leCRLF {
		t.Fatalf("detected %v", le)
	}
	lf := normalizeToLF(crlf)
	if lf != "a\nb\n" {
		t.Fatalf("normalized = %q", lf)
	}
	if got := restoreLineEndings(lf, le); got != crlf {
		t.Fatalf("restored = %q", got)
	}
}

func TestEditToolCRLFFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.go")
	if err := os.WriteFile(path, []byte("package main\r\n\r\nfunc main() {\r\n}\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := (EditTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{
		"path":  "file.go",
		"edits": []any{map[string]any{"oldText": "func main() {", "newText": "func main() int {"}},
	}))
	if res.IsError {
		t.Fatalf("edit failed: %s", blockText(res))
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if !strings.Contains(s, "func main() int {") {
		t.Fatalf("content = %q", s)
	}
	if !strings.Contains(s, "\r\n") {
		t.Fatal("CRLF line endings lost")
	}
}

func TestEditToolArgRepairs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	os.WriteFile(path, []byte("alpha beta\n"), 0o644) //nolint:errcheck

	// edits as JSON string (the model quoted the array)
	res := (EditTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{
		"path":  "f.txt",
		"edits": `[{"oldText":"alpha","newText":"gamma"}]`,
	}))
	if res.IsError {
		t.Fatalf("string-form edits rejected: %s", blockText(res))
	}

	// single edit object
	os.WriteFile(path, []byte("alpha beta\n"), 0o644) //nolint:errcheck
	res = (EditTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{
		"path":  "f.txt",
		"edits": map[string]any{"oldText": "alpha", "newText": "delta"},
	}))
	if res.IsError {
		t.Fatalf("single-object edits rejected: %s", blockText(res))
	}

	// flat legacy form
	os.WriteFile(path, []byte("alpha beta\n"), 0o644) //nolint:errcheck
	res = (EditTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{
		"path":    "f.txt",
		"oldText": "alpha",
		"newText": "epsilon",
	}))
	if res.IsError {
		t.Fatalf("flat edits rejected: %s", blockText(res))
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "epsilon") {
		t.Fatalf("content = %s", data)
	}
}

func TestEditToolChineseExact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zh.txt")
	os.WriteFile(path, []byte("第一行\n第二行内容\n第三行\n"), 0o644) //nolint:errcheck
	res := (EditTool{}).Execute(agent.ToolContext{CWD: dir}, mustArgs(t, map[string]any{
		"path":  "zh.txt",
		"edits": []any{map[string]any{"oldText": "第二行内容", "newText": "第二行已修改"}},
	}))
	if res.IsError {
		t.Fatalf("edit failed: %s", blockText(res))
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "第二行已修改") {
		t.Fatalf("content = %s", data)
	}
}

func mustArgs(t *testing.T, m map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func blockText(res agent.ToolResult) string {
	var sb strings.Builder
	for _, b := range res.Content {
		if b.Kind == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

func TestUnifiedDiff(t *testing.T) {
	diff := UnifiedDiff("a\nb\nc\n", "a\nx\nc\n", "f.txt")
	if !strings.Contains(diff, "-b") || !strings.Contains(diff, "+x") || !strings.Contains(diff, "@@") {
		t.Fatalf("diff = %s", diff)
	}
	if UnifiedDiff("same\n", "same\n", "f") != "" {
		t.Fatal("identical content must produce no diff")
	}
}
