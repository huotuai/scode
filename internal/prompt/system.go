// Package prompt assembles scode's system prompt from stable sections
// (pi's system-prompt.ts design). Cache rule: no dates, no git status,
// no model names inside the prompt — volatile facts reach the model via
// shell tool environment variables (SCODE_MODEL & co).
package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"scode/internal/llm"
)

// Section is one named prompt slice; sections replay deterministically
// through the transcript model.
type Section struct {
	Name  string
	Value string
}

// Instruction file candidates per directory, first existing wins
// (pi's resource-loader order).
var instructionCandidates = []string{"AGENTS.override.md", "AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD"}

// FindInstructions returns the instruction files for a working
// directory: the global file (configDir/AGENTS.md) first, then one per
// ancestor directory ordered root -> cwd, deduplicated.
func FindInstructions(configDir, cwd string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(path string) {
		if !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}

	if p := filepath.Join(configDir, "AGENTS.md"); fileReadable(p) {
		add(p)
	}

	// Walk cwd up to the root collecting candidates, then emit root-first.
	var chain []string
	dir := filepath.Clean(cwd)
	for {
		if p := firstCandidate(dir); p != "" {
			chain = append(chain, p)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	for i := len(chain) - 1; i >= 0; i-- {
		add(chain[i])
	}
	return out
}

func firstCandidate(dir string) string {
	for _, name := range instructionCandidates {
		p := filepath.Join(dir, name)
		if fileReadable(p) {
			return p
		}
	}
	return ""
}

func fileReadable(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// Build assembles the system prompt message: base preamble plus named
// sections (rules, cwd, project_context) in a fixed order.
func Build(configDir, cwd string, extra []Section) llm.Message {
	sections := []Section{
		{Name: "rules", Value: rulesText},
		{Name: "cwd", Value: formatCWD(cwd)},
	}

	var ctxParts []string
	for _, p := range FindInstructions(configDir, cwd) {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		rel, relErr := filepath.Rel(cwd, p)
		display := p
		if relErr == nil && !strings.HasPrefix(rel, "..") {
			display = rel
		}
		ctxParts = append(ctxParts, fmt.Sprintf("<project_instructions path=%q>\n%s\n</project_instructions>", display, strings.TrimSpace(string(data))))
	}
	if len(ctxParts) > 0 {
		sections = append(sections, Section{Name: "project_context", Value: strings.Join(ctxParts, "\n\n")})
	}

	sections = append(sections, extra...)

	m := llm.Message{
		Role:    llm.RoleSystem,
		Content: []llm.Block{llm.TextBlock(preamble)},
	}
	for _, s := range sections {
		if strings.TrimSpace(s.Value) == "" {
			continue
		}
		m.Sections = append(m.Sections, llm.Section{Name: s.Name, Value: s.Value})
	}
	return m
}

const preamble = `You are scode, an expert coding assistant operating in a terminal. You help with software engineering tasks: reading and editing code, running commands, and answering questions about the codebase. Be concise and precise. State what you did and what remains. When a task needs a decision you cannot make, ask.`

const rulesText = `- Use the read tool before editing files you have not seen this session.
- Prefer edit over write for existing files; write only for new files or full rewrites.
- Run commands through the bash tool; check exit codes and output before claiming success.
- Keep tool outputs in mind: grep and find before searching by hand.
- When a tool call fails, read the error and self-correct instead of repeating the same call.`

func formatCWD(cwd string) string {
	// Forward slashes keep prompts stable across platforms.
	return filepath.ToSlash(filepath.Clean(cwd))
}

// SortStableSections orders extra sections by name for determinism.
func SortStableSections(secs []Section) []Section {
	out := append([]Section(nil), secs...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
