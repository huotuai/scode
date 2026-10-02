package tools

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"scode/internal/agent"
	"scode/internal/llm"
)

// GrepTool searches file content (pi grep.ts schema: pattern / path? /
// glob? / ignoreCase? / literal? / context? / limit, default 100
// matches). Pure-Go walk + regexp; respects .gitignore and skips junk
// and binary-looking files. An rg-backed fast path arrives later — the
// schema and output shape are already aligned with it.
type GrepTool struct{}

func (GrepTool) Decl() llm.Tool {
	return llm.Tool{
		Name:        "grep",
		Description: "Search file contents with a regular expression (or literal text). Returns file:line: text matches, respecting .gitignore. With filesOnly=true, returns only the paths of matching files, one per line — a cheap way to shortlist candidates before reading.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"Regex pattern (or literal text with literal=true)"},"path":{"type":"string","description":"File or directory to search (default: working directory)"},"glob":{"type":"string","description":"Restrict to files matching this glob, e.g. *.go"},"ignoreCase":{"type":"boolean","description":"Case-insensitive matching"},"literal":{"type":"boolean","description":"Treat pattern as literal text"},"context":{"type":"integer","description":"Lines of context around each match"},"filesOnly":{"type":"boolean","description":"Return only matching file paths, one per line"},"limit":{"type":"integer","description":"Maximum matches (default 100)"}},"required":["pattern"]}`),
	}
}

// grepHit is one formatted match group (match + context lines).
type grepHit struct{ text string }

func (GrepTool) Execute(tc agent.ToolContext, args json.RawMessage) agent.ToolResult {
	var a struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Glob       string `json:"glob"`
		IgnoreCase bool   `json:"ignoreCase"`
		Literal    bool   `json:"literal"`
		Context    int    `json:"context"`
		FilesOnly  bool   `json:"filesOnly"`
		Limit      int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return agent.ErrorResult("invalid arguments: " + err.Error())
	}
	if a.Pattern == "" {
		return agent.ErrorResult("pattern is required")
	}
	pattern := a.Pattern
	if a.Literal {
		pattern = regexp.QuoteMeta(pattern)
	}
	flags := ""
	if a.IgnoreCase {
		flags = "(?i)"
	}
	re, err := regexp.Compile(flags + pattern)
	if err != nil {
		return agent.ErrorResult(fmt.Sprintf("invalid pattern: %v", err))
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 100
	}

	root := Resolve(tc, a.Path)
	info, err := os.Stat(root)
	if err != nil {
		return agent.ErrorResult(fmt.Sprintf("cannot stat %s: %v", a.Path, err))
	}

	// rg fast path (rg.go): same schema and output shape, native
	// nested-.gitignore handling. Any rg-side failure falls through to
	// the pure-Go walker below.
	if bin := rgBinary(); bin != "" {
		if res, ok := rgSearch(tc, root, info, rgOptions{
			pattern:    pattern,
			ignoreCase: a.IgnoreCase,
			context:    a.Context,
			glob:       a.Glob,
			limit:      limit,
			filesOnly:  a.FilesOnly,
		}); ok {
			return res
		}
	}

	if a.FilesOnly {
		return grepFilesOnly(root, a.Path, info, re, a.Glob, limit)
	}

	var hits []grepHit
	add := func(h grepHit) {
		hits = append(hits, h)
	}

	if !info.IsDir() {
		scanFile(root, "", re, a.Context, limit, add)
	} else {
		ign := LoadIgnoreMatcher(root)
		budget := limit + limit/2 // overshoot allowance for context lines
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return nil
			}
			relSlash := filepath.ToSlash(rel)
			if d.IsDir() {
				if junkDirs[d.Name()] || ign.Match(relSlash) {
					return filepath.SkipDir
				}
				return nil
			}
			if ign.Match(relSlash) {
				return nil
			}
			if a.Glob != "" {
				if !MatchGlob(a.Glob, relSlash) {
					return nil
				}
			}
			scanFile(path, relSlash, re, a.Context, budget-len(hits), add)
			if len(hits) >= budget {
				return filepath.SkipAll
			}
			return nil
		})
	}

	if len(hits) == 0 {
		return agent.TextResult("No matches found.")
	}
	capped := hits
	if len(capped) > limit {
		capped = capped[:limit]
	}
	tr := TruncateHead(strings.Join(func() []string {
		out := make([]string, len(capped))
		for i, h := range capped {
			out[i] = h.text
		}
		return out
	}(), "\n"))
	out := tr.Text
	if tr.Truncated {
		out += "\n" + tr.Notice
	}
	if len(hits) > limit {
		out += fmt.Sprintf("\n[Showing %d of %d+ matches — narrow the pattern or path]", limit, len(hits))
	}
	return agent.TextResult(out)
}

// scanFile appends formatted matches (with context lines) for one file.
func scanFile(path, display string, re *regexp.Regexp, context, budget int, add func(grepHit)) {
	if budget <= 0 {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > 400_000 { // pathological file guard
			break
		}
	}
	if sc.Err() != nil || looksBinary(lines) {
		return
	}
	if display == "" {
		display = path
	}

	shown := map[int]bool{}
	var matched []int
	for i, l := range lines {
		if re.MatchString(l) {
			matched = append(matched, i)
			for c := i - context; c <= i+context; c++ {
				if c >= 0 && c < len(lines) {
					shown[c] = true
				}
			}
			if len(matched) >= budget {
				break
			}
		}
	}
	for _, i := range matched {
		var sb strings.Builder
		for c := i - context; c <= i+context; c++ {
			if c < 0 || c >= len(lines) || !shown[c] {
				continue
			}
			// Match lines read "path:line: text"; context lines read
			// "path-line- text" (grep convention — the separator tells
			// matches from context).
			if c == i {
				sb.WriteString(fmt.Sprintf("> %s:%d: %s\n", display, c+1, ClampLine(lines[c])))
			} else {
				sb.WriteString(fmt.Sprintf("  %s-%d- %s\n", display, c+1, ClampLine(lines[c])))
			}
		}
		add(grepHit{strings.TrimRight(sb.String(), "\n")})
	}
}

// grepFilesOnly implements the filesOnly mode: same walk rules as the
// match mode (junk dirs, .gitignore, glob, binary skip), but collects
// one path per matching file. Output stays small even for broad
// patterns, so the model can shortlist candidates before reading.
func grepFilesOnly(root, argPath string, info os.FileInfo, re *regexp.Regexp, glob string, limit int) agent.ToolResult {
	var files []string
	if !info.IsDir() {
		if fileHasMatch(root, re) {
			files = append(files, argPath)
		}
	} else {
		ign := LoadIgnoreMatcher(root)
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return nil
			}
			relSlash := filepath.ToSlash(rel)
			if d.IsDir() {
				if junkDirs[d.Name()] || ign.Match(relSlash) {
					return filepath.SkipDir
				}
				return nil
			}
			if ign.Match(relSlash) {
				return nil
			}
			if glob != "" && !MatchGlob(glob, relSlash) {
				return nil
			}
			if fileHasMatch(path, re) {
				files = append(files, relSlash)
				if len(files) >= limit {
					return filepath.SkipAll
				}
			}
			return nil
		})
	}

	if len(files) == 0 {
		return agent.TextResult("No matches found.")
	}
	tr := TruncateHead(strings.Join(files, "\n"))
	out := tr.Text
	if tr.Truncated {
		out += "\n" + tr.Notice
	}
	if len(files) >= limit {
		out += fmt.Sprintf("\n[Showing %d files at the limit — narrow the pattern or path]", limit)
	}
	return agent.TextResult(out)
}

// fileHasMatch reports whether the file has at least one matching
// line, with the same binary-file guard as scanFile. Early-exits on
// the first match, so shortlisting a large tree stays cheap.
func fileHasMatch(path string, re *regexp.Regexp) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	var head []string
	n := 0
	for sc.Scan() {
		line := sc.Text()
		n++
		if len(head) < 64 {
			head = append(head, line)
			if looksBinary(head) && len(head) == 64 {
				return false
			}
		}
		if re.MatchString(line) {
			return !looksBinary(head)
		}
		if n > 400_000 { // pathological file guard
			break
		}
	}
	return false
}

// looksBinary heuristics: NUL byte or overwhelming non-text ratio in the
// first lines.
func looksBinary(lines []string) bool {
	n := len(lines)
	if n > 64 {
		n = 64
	}
	for i := 0; i < n; i++ {
		if strings.ContainsRune(lines[i], 0) {
			return true
		}
	}
	return false
}

// PromptContribution is pi's grepToolSystemPromptContribution, plus
// search-strategy guidelines: grep is the primary locator — narrow
// before wide, and let match line numbers drive windowed reads.
func (GrepTool) PromptContribution() (string, []string) {
	return "Search file contents for patterns (respects .gitignore)", []string{
		"Locate code with grep (content) or find (path) first instead of listing directories and reading files one by one.",
		"Start narrow: scope grep with path/glob; only widen the search when a narrow one finds nothing.",
		"If grep returns too many hits, narrow the pattern, path, or glob instead of paging through matches.",
		"Use grep with filesOnly=true to shortlist candidate files for a broad pattern before drilling into matches or reading.",
	}
}
