package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"scode/internal/agent"
)

// rg.go is the ripgrep fast path for GrepTool (the pure-Go walker in
// grep.go stays the fallback — the schema and output shape are shared).
// rg brings native nested-.gitignore handling, .ignore files, and
// orders-of-magnitude faster tree scans; when rg is absent or fails to
// start, the caller silently falls back to the pure-Go path.

// rgLookPath is a test hook for the rg probe.
var rgLookPath = exec.LookPath

var (
	rgOnce sync.Once
	rgPath string
)

// rgBinary resolves the ripgrep executable once per process; "" means
// unavailable and the caller must use the pure-Go fallback.
func rgBinary() string {
	rgOnce.Do(func() {
		if p, err := rgLookPath("rg"); err == nil {
			rgPath = p
		}
	})
	return rgPath
}

// rgTimeout bounds one search. rg scans even large trees in seconds;
// the cap exists for pathological filesystems (network drives).
const rgTimeout = 60 * time.Second

// rgOptions carries the translated grep arguments.
type rgOptions struct {
	pattern    string // final pattern text (already literal-quoted if needed)
	ignoreCase bool
	context    int
	glob       string
	limit      int
	filesOnly  bool
}

// rgSearch runs ripgrep and formats its output exactly like the pure-Go
// path. ok=false tells the caller to fall back (spawn failure, rg
// error exit, timeout); rg's exit code 1 (no matches) is ok=true.
func rgSearch(tc agent.ToolContext, root string, info os.FileInfo, o rgOptions) (res agent.ToolResult, ok bool) {
	args := []string{"--no-config", "--hidden", "--sort", "path"}
	// Keep parity with junkDirs: rg honors .gitignore but a repo
	// without one would still descend into node_modules & co.
	names := make([]string, 0, len(junkDirs))
	for n := range junkDirs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		args = append(args, "-g", "!"+n+"/", "-g", "!**/"+n+"/")
	}
	if o.ignoreCase {
		args = append(args, "-i")
	}
	if o.filesOnly {
		args = append(args, "--files-with-matches")
	} else {
		args = append(args, "--json")
		if o.context > 0 {
			args = append(args, "-C", strconv.Itoa(o.context))
		}
	}
	if o.glob != "" {
		args = append(args, "-g", o.glob)
	}

	dir := root
	target := "."
	if !info.IsDir() {
		// Single file: search it by absolute path; display stays
		// absolute, matching the pure-Go single-file branch.
		dir = filepath.Dir(root)
		target = root
	}
	args = append(args, "--", o.pattern, target)

	ctx := tc.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, rgTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, rgPath, args...)
	cmd.Dir = dir
	out, err := cmd.StdoutPipe()
	if err != nil {
		return agent.ToolResult{}, false
	}
	if err := cmd.Start(); err != nil {
		return agent.ToolResult{}, false
	}

	var lines []string
	matchCount := 0
	cut := false
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 256*1024), 16<<20)
	for sc.Scan() {
		raw := sc.Text()
		if raw == "" {
			continue
		}
		if o.filesOnly {
			lines = append(lines, rgDisplayPath(raw, target))
			if len(lines) >= o.limit {
				cut = true
				break
			}
			continue
		}
		formatted, isMatch := rgFormatEvent(raw, target)
		if formatted == "" {
			continue
		}
		if isMatch {
			matchCount++
			if matchCount > o.limit {
				// The cut point sits BEFORE this match, so all
				// context lines of the kept matches are already in.
				cut = true
				break
			}
		}
		lines = append(lines, formatted)
	}
	if cut {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return agent.ToolResult{}, false // timeout: pure-Go fallback
	}
	if waitErr != nil && !cut {
		if exit, ok2 := waitErr.(*exec.ExitError); ok2 && exit.ExitCode() == 1 {
			return agent.TextResult("No matches found."), true
		}
		return agent.ToolResult{}, false
	}

	if len(lines) == 0 {
		return agent.TextResult("No matches found."), true
	}
	tr := TruncateHead(strings.Join(lines, "\n"))
	text := tr.Text
	if tr.Truncated {
		text += "\n" + tr.Notice
	}
	if cut {
		if o.filesOnly {
			text += fmt.Sprintf("\n[Showing %d files at the limit — narrow the pattern or path]", o.limit)
		} else {
			text += fmt.Sprintf("\n[Showing %d of %d+ matches — narrow the pattern or path]", o.limit, o.limit)
		}
	}
	return agent.TextResult(text), true
}

// rgEvent is the subset of rg's --json message stream we consume.
type rgEvent struct {
	Type string `json:"type"`
	Data struct {
		Path struct {
			Text string `json:"text"`
		} `json:"path"`
		Lines struct {
			Text string `json:"text"`
		} `json:"lines"`
		LineNumber int `json:"line_number"`
	} `json:"data"`
}

// rgFormatEvent converts one rg --json message into the grep output
// shape: "> path:line: text" for matches, "  path-line- text" for
// context. "" means the message carries no displayable line (begin /
// end / summary) and must be skipped.
func rgFormatEvent(raw, target string) (line string, isMatch bool) {
	var ev rgEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		return "", false
	}
	if ev.Type != "match" && ev.Type != "context" {
		return "", false
	}
	display := rgDisplayPath(ev.Data.Path.Text, target)
	text := ClampLine(strings.TrimRight(ev.Data.Lines.Text, "\n"))
	if ev.Type == "match" {
		return fmt.Sprintf("> %s:%d: %s", display, ev.Data.LineNumber, text), true
	}
	return fmt.Sprintf("  %s-%d- %s", display, ev.Data.LineNumber, text), false
}

// rgDisplayPath normalizes an rg-reported path to the relative slash
// form the pure-Go walker emits. A "." search target yields "./x" or
// ".\x" prefixes depending on platform; an absolute target (single
// file) is passed through unchanged.
func rgDisplayPath(p, target string) string {
	if target == "." {
		p = strings.TrimPrefix(p, "./")
		p = strings.TrimPrefix(p, ".\\")
	}
	return filepath.ToSlash(p)
}
