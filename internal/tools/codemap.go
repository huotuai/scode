package tools

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"scode/internal/agent"
	"scode/internal/llm"
)

// CodeMapTool extracts a structural outline — top-level symbols with
// line numbers — from source files, so the model can map a package
// without reading whole files (the Aider repo-map idea, regex-based:
// no tree-sitter dependency). Supported languages are matched by
// extension; unknown extensions are skipped silently.
type CodeMapTool struct{}

// codemapMaxFiles caps how many files one call outlines (dir mode).
const codemapMaxFiles = 100

// codemapMaxFileSize skips files too large to be source hand-edited
// (generated bundles, minified assets).
const codemapMaxFileSize = 2 << 20

// codemapMaxLineLen clamps one signature line.
const codemapMaxLineLen = 160

func (CodeMapTool) Decl() llm.Tool {
	return llm.Tool{
		Name:        "codemap",
		Description: "Extract a structural outline of source files — top-level symbols (types, functions, classes) with line numbers — without returning full file contents. Pass a file or directory. A cheap way to map a package before deciding what to read. Supported: Go, TypeScript/JavaScript, Python, Rust.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"path":{"type":"string","description":"Source file or directory (default: working directory)"},` +
			`"glob":{"type":"string","description":"Restrict to files matching this glob, e.g. *.go"},` +
			`"limit":{"type":"integer","description":"Maximum files to outline (default 100)"}` +
			`},"required":[]}`),
	}
}

// symbolPatterns maps a file extension to the line-oriented regexes
// that match its top-level declarations. All patterns anchor at
// column 0, which naturally excludes nested/local declarations for
// brace languages and non-top-level defs for Python.
var symbolPatterns = map[string][]*regexp.Regexp{
	".go": {
		regexp.MustCompile(`^func\s+(\(.*\)\s*)?\w+\s*\(`),
		regexp.MustCompile(`^type\s+\w+\s`),
	},
	".ts":  tsSymbolPatterns,
	".tsx": tsSymbolPatterns,
	".js":  tsSymbolPatterns,
	".jsx": tsSymbolPatterns,
	".mjs": tsSymbolPatterns,
	".cjs": tsSymbolPatterns,
	".py": {
		regexp.MustCompile(`^(async\s+)?def\s+\w+\s*\(`),
		regexp.MustCompile(`^class\s+\w+`),
	},
	".rs": {
		regexp.MustCompile(`^(pub(\(.*\))?\s+)?(async\s+)?(unsafe\s+)?fn\s+\w+`),
		regexp.MustCompile(`^(pub\s+)?(struct|enum|trait|impl|mod)\s+\w+`),
	},
}

var tsSymbolPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^(export\s+)?(default\s+)?(async\s+)?function\s*\*?\s*\w+`),
	regexp.MustCompile(`^(export\s+)?(default\s+)?(abstract\s+)?class\s+\w+`),
	regexp.MustCompile(`^(export\s+)?(interface|type|enum)\s+\w+`),
	// Top-level arrow/function constants: const foo = ( | async | function
	regexp.MustCompile(`^(export\s+)?const\s+\w+\s*(:\s*[^=]+)?=\s*(\(|async\s|function\s*[\w(])`),
}

// outlineFile extracts the symbol outline of one source file. A nil
// result means "not outlineable" (unknown extension, too large,
// unreadable, or no symbols) — the caller skips the file.
func outlineFile(path string) []string {
	ext := strings.ToLower(filepath.Ext(path))
	pats, ok := symbolPatterns[ext]
	if !ok {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() > codemapMaxFileSize {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var symbols []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		for _, p := range pats {
			if !p.MatchString(line) {
				continue
			}
			sig := strings.TrimSpace(line)
			sig = strings.TrimRight(strings.TrimSpace(strings.TrimSuffix(sig, "{")), " ")
			if len(sig) > codemapMaxLineLen {
				sig = sig[:codemapMaxLineLen] + "…"
			}
			symbols = append(symbols, fmt.Sprintf("  %d: %s", lineNo, sig))
			break
		}
	}
	return symbols
}

func (CodeMapTool) Execute(tc agent.ToolContext, args json.RawMessage) agent.ToolResult {
	var a struct {
		Path  string `json:"path"`
		Glob  string `json:"glob"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return agent.ErrorResult("invalid arguments: " + err.Error())
	}
	limit := a.Limit
	if limit <= 0 {
		limit = codemapMaxFiles
	}

	root := Resolve(tc, a.Path)
	info, err := os.Stat(root)
	if err != nil {
		return agent.ErrorResult(fmt.Sprintf("cannot stat %s: %v", a.Path, err))
	}

	var files []string
	if !info.IsDir() {
		files = append(files, a.Path)
		if outlineFile(root) == nil {
			return agent.ErrorResult(fmt.Sprintf("no outlineable symbols in %s (unsupported extension or no top-level declarations; supported: .go .ts .tsx .js .jsx .mjs .cjs .py .rs)", a.Path))
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
			if a.Glob != "" && !MatchGlob(a.Glob, relSlash) {
				return nil
			}
			if _, ok := symbolPatterns[strings.ToLower(filepath.Ext(path))]; !ok {
				return nil
			}
			files = append(files, relSlash)
			if len(files) >= limit {
				return filepath.SkipAll
			}
			return nil
		})
		sort.Strings(files)
	}

	var b strings.Builder
	outlined := 0
	for _, rel := range files {
		abs := rel
		if info.IsDir() {
			abs = filepath.Join(root, rel)
		}
		symbols := outlineFile(abs)
		if len(symbols) == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n') // blank line between file blocks
		}
		b.WriteString(rel + ":\n")
		b.WriteString(strings.Join(symbols, "\n"))
		b.WriteByte('\n')
		outlined++
	}
	if outlined == 0 {
		return agent.TextResult("No outlineable symbols found (supported: .go .ts .tsx .js .jsx .mjs .cjs .py .rs).")
	}
	tr := TruncateHead(strings.TrimRight(b.String(), "\n"))
	out := tr.Text
	if tr.Truncated {
		out += "\n" + tr.Notice
	}
	if info.IsDir() && len(files) >= limit {
		out += fmt.Sprintf("\n[Showing %d files at the limit — narrow the path or glob]", limit)
	}
	return agent.TextResult(out)
}

// PromptContribution positions codemap as the structure-first step:
// outline a package before choosing what to read in full.
func (CodeMapTool) PromptContribution() (string, []string) {
	return "Outline source files' top-level symbols (structure map)", []string{
		"Use codemap on a directory or file to see its top-level symbols before reading whole files — outline first, then read only the windows you need.",
	}
}
