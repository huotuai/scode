package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"scode/internal/agent"
	"scode/internal/llm"
)

// LsTool lists directory entries (pi ls.ts: path? / limit, default 500),
// alphabetical, directories suffixed with /, dotfiles included.
type LsTool struct{}

func (LsTool) Decl() llm.Tool {
	return llm.Tool{
		Name:        "ls",
		Description: "List a directory's entries, alphabetical, directories suffixed with /. Includes dotfiles.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Directory path (default: working directory)"},"limit":{"type":"integer","description":"Maximum entries to return (default 500)"}}}`),
	}
}

func (LsTool) Execute(tc agent.ToolContext, args json.RawMessage) agent.ToolResult {
	var a struct {
		Path  string `json:"path"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return agent.ErrorResult("invalid arguments: " + err.Error())
	}
	dir := Resolve(tc, a.Path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return agent.ErrorResult(fmt.Sprintf("cannot list %s: %v", a.Path, err))
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)

	limit := a.Limit
	if limit <= 0 {
		limit = 500
	}
	if len(names) > limit {
		names = names[:limit]
	}
	tr := TruncateHead(strings.Join(names, "\n"))
	out := tr.Text
	if tr.Truncated {
		out += "\n" + tr.Notice
	}
	if len(entries) > limit {
		out += fmt.Sprintf("\n[Showing %d of %d entries]", limit, len(entries))
	}
	return agent.TextResult(out)
}

// FindTool walks the tree matching a glob (pi find.ts: pattern / path? /
// limit, default 1000), respecting .gitignore semantics via doublestar
// patterns; directories named .git are skipped.
type FindTool struct{}

func (FindTool) Decl() llm.Tool {
	return llm.Tool{
		Name:        "find",
		Description: "Find files matching a glob pattern (e.g. **/*.go), recursively, skipping .git and common junk directories. Respects .gitignore.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"Glob pattern, e.g. **/*.ts or src/**/*.go"},"path":{"type":"string","description":"Root directory (default: working directory)"},"limit":{"type":"integer","description":"Maximum matches (default 1000)"}},"required":["pattern"]}`),
	}
}

var junkDirs = map[string]bool{
	".git": true, "node_modules": true, ".hg": true, ".svn": true,
	"dist": true, "build": true, ".venv": true, "__pycache__": true,
}

func (FindTool) Execute(tc agent.ToolContext, args json.RawMessage) agent.ToolResult {
	var a struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Limit   int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return agent.ErrorResult("invalid arguments: " + err.Error())
	}
	if a.Pattern == "" {
		return agent.ErrorResult("pattern is required")
	}
	root := Resolve(tc, a.Path)
	limit := a.Limit
	if limit <= 0 {
		limit = 1000
	}

	gitignored := LoadIgnoreMatcher(root)
	var matches []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip, keep walking
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		if d.IsDir() {
			if junkDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if gitignored.Match(relSlash) {
			return nil
		}
		if ok, _ := doublestarMatch(a.Pattern, relSlash); ok {
			matches = append(matches, relSlash)
			if len(matches) >= limit {
				return filepath.SkipAll
			}
		}
		return nil
	})
	if err != nil {
		return agent.ErrorResult("walk failed: " + err.Error())
	}
	if len(matches) == 0 {
		return agent.TextResult("No matches found.")
	}
	tr := TruncateHead(strings.Join(matches, "\n"))
	out := tr.Text
	if tr.Truncated {
		out += "\n" + tr.Notice
	}
	return agent.TextResult(out)
}
