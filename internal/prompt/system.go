// Package prompt assembles scode's system prompt from stable sections
// (pi's buildSystemPromptSections). Cache rule: no dates, no git
// status, no model names inside the prompt — volatile facts reach the
// model via shell tool environment variables (SCODE_*).
package prompt

import (
	"os"
	"path/filepath"
	"strings"

	"scode/internal/agent"
	"scode/internal/llm"
	"scode/internal/skills"
)

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

// preamble is pi's default preamble, branded for scode.
const preamble = `You are an expert coding assistant operating inside scode, a coding agent harness. You help users by reading files, executing commands, editing code, and writing new files.`

// Build assembles the system prompt message (pi's
// buildSystemPromptSections): preamble, then tagged sections tools,
// rules, project_context, skills, cwd — in pi's fixed order.
func Build(configDir, cwd string, contribs []agent.ToolContribution, skillList []skills.Skill) llm.Message {
	sections := []llm.Section{}

	// tools: the visible tool list with one-line snippets (pi).
	var tb strings.Builder
	for _, c := range contribs {
		if c.Snippet == "" {
			continue
		}
		tb.WriteString("- " + c.Name + ": " + c.Snippet + "\n")
	}
	tools := strings.TrimRight(tb.String(), "\n")
	if tools == "" {
		tools = "(none)"
	}
	tools += "\n\nIn addition to the tools above, you may have access to other custom tools depending on the project."
	sections = append(sections, llm.Section{Name: "tools", Value: tools})

	// rules: deduped per-tool guidelines plus pi's trailing rules
	// (the bash-file-ops rule is skipped: scode always has grep/find/ls).
	sections = append(sections, llm.Section{Name: "rules", Value: buildRules(contribs)})

	// project_context: instruction files (pi's renderProjectContext).
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
		ctxParts = append(ctxParts, "<project_instructions path=\""+display+"\">\n"+strings.TrimSpace(string(data))+"\n</project_instructions>")
	}
	if len(ctxParts) > 0 {
		value := "Project-specific instructions and guidelines:\n\n" + strings.Join(ctxParts, "\n\n")
		sections = append(sections, llm.Section{Name: "project_context", Value: value})
	}

	// skills: the <available_skills> listing (pi's formatSkillsForPrompt,
	// trimmed before sectioning). scode always has the read tool, so the
	// listing is emitted whenever skills are visible.
	if sp := strings.TrimSpace(skills.FormatForPrompt(skillList)); sp != "" {
		sections = append(sections, llm.Section{Name: "skills", Value: sp})
	}

	sections = append(sections, llm.Section{Name: "cwd", Value: filepath.ToSlash(filepath.Clean(cwd))})

	m := llm.Message{
		Role:     llm.RoleSystem,
		Content:  []llm.Block{llm.TextBlock(preamble)},
		Sections: sections,
	}
	return m
}

// buildRules is pi's buildRules: dedup in order, per-tool guidelines,
// then the fixed trailing rules.
func buildRules(contribs []agent.ToolContribution) string {
	var rules []string
	seen := map[string]bool{}
	add := func(rule string) {
		rule = strings.TrimSpace(rule)
		if rule == "" || seen[rule] {
			return
		}
		seen[rule] = true
		rules = append(rules, rule)
	}

	// pi adds "Use bash for file operations" only when grep/find/ls are
	// absent — scode always ships them, so that rule never applies.
	for _, c := range contribs {
		for _, g := range c.Guidelines {
			add(g)
		}
	}
	add("Be concise in your responses")
	add("Show file paths clearly when working with files")
	add("Issue independent exploration calls (grep/find/read) in the same turn rather than one at a time.")

	var b strings.Builder
	for _, r := range rules {
		b.WriteString("- " + r + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
