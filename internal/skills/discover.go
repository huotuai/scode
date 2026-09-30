package skills

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// DiscoveryMode selects the bare-.md inclusion rule (pi's
// SkillDiscoveryMode in package-manager.ts):
//
//   - ModePi (.scode/skills): direct .md children of the root count
//   - ModeAgents (.agents/skills): only .md files BELOW the root count
//     (the skills root itself may still be a SKILL.md skill root)
type DiscoveryMode int

const (
	ModePi DiscoveryMode = iota
	ModeAgents
)

// ignoreFileNames are the per-directory ignore files pi honors.
var ignoreFileNames = []string{".gitignore", ".ignore", ".fdignore"}

// LoadFromDir discovers skills under dir (pi's loadSkillsFromDir +
// collectSkillEntries):
//
//   - if dir contains SKILL.md, dir IS the skill root: load it and stop
//     (no recursion)
//   - otherwise, include direct .md children per the discovery mode
//   - recurse into subdirectories (skipping dot dirs and node_modules)
//     looking for SKILL.md roots and, below the root, .md files
//
// Ignore rules from .gitignore/.ignore/.fdignore accumulate down the
// tree, scoped to the directory that declares them (pi's
// prefixIgnorePattern).
func LoadFromDir(dir, source string, mode DiscoveryMode) LoadResult {
	return loadFromDir(dir, source, mode, nil, filepath.Clean(dir))
}

func loadFromDir(dir, source string, mode DiscoveryMode, ig *ignoreMatcher, root string) LoadResult {
	var result LoadResult
	entries, err := os.ReadDir(dir)
	if err != nil {
		return result // missing/unreadable dirs yield nothing (pi: existsSync + try/catch)
	}
	if ig == nil {
		ig = &ignoreMatcher{}
	}
	ig.addRules(dir, root)

	// A SKILL.md here makes dir the skill root: load and stop (pi
	// returns immediately).
	for _, entry := range entries {
		if entry.Name() != "SKILL.md" {
			continue
		}
		full := filepath.Join(dir, entry.Name())
		if !isFile(entry, full) || ig.ignores(relSlash(root, full)) {
			break
		}
		skill, diags := loadSkillFromFile(full, source)
		result.Diagnostics = append(result.Diagnostics, diags...)
		if skill != nil {
			result.Skills = append(result.Skills, *skill)
		}
		return result
	}

	isRoot := filepath.Clean(dir) == root
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue // dotfiles/dot dirs are never scanned (pi)
		}
		if name == "node_modules" {
			continue
		}
		full := filepath.Join(dir, name)

		isDir, isF := classifyEntry(entry, full)
		rel := relSlash(root, full)
		ignorePath := rel
		if isDir {
			ignorePath = rel + "/"
		}
		if ig.ignores(ignorePath) {
			continue
		}

		if isDir {
			sub := loadFromDir(full, source, mode, ig.fork(), root)
			result.Skills = append(result.Skills, sub.Skills...)
			result.Diagnostics = append(result.Diagnostics, sub.Diagnostics...)
			continue
		}
		if !isF || !strings.HasSuffix(name, ".md") {
			continue
		}
		// pi's shouldIncludeMarkdownFile: pi mode takes root-level .md
		// files, agents mode only takes .md files below the root.
		if (mode == ModePi && isRoot) || (mode == ModeAgents && !isRoot) {
			skill, diags := loadSkillFromFile(full, source)
			result.Diagnostics = append(result.Diagnostics, diags...)
			if skill != nil {
				result.Skills = append(result.Skills, *skill)
			}
		}
	}
	return result
}

// classifyEntry resolves symlinks (pi's statSync follow); broken links
// report nothing.
func classifyEntry(entry os.DirEntry, full string) (isDir, isFile bool) {
	if entry.Type()&os.ModeSymlink != 0 {
		st, err := os.Stat(full)
		if err != nil {
			return false, false
		}
		return st.IsDir(), st.Mode().IsRegular()
	}
	return entry.IsDir(), entry.Type().IsRegular()
}

func isFile(entry os.DirEntry, full string) bool {
	_, f := classifyEntry(entry, full)
	return f
}

// relSlash renders p root-relative with forward slashes (pi's
// toPosixPath(relative(...))).
func relSlash(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return filepath.ToSlash(p)
	}
	return filepath.ToSlash(rel)
}

// ---------------------------------------------------------------------------
// Ignore matching (pi's `ignore` package usage, approximated with
// doublestar — the same conventions as scode's tool-side matcher)
// ---------------------------------------------------------------------------

type ignoreRule struct {
	pattern string
	negate  bool
}

type ignoreMatcher struct {
	rules []ignoreRule
}

func (m *ignoreMatcher) fork() *ignoreMatcher {
	out := &ignoreMatcher{rules: append([]ignoreRule{}, m.rules...)}
	return out
}

// addRules appends the ignore rules declared in dir, prefixed to their
// declaring directory (pi's addIgnoreRules + prefixIgnorePattern).
func (m *ignoreMatcher) addRules(dir, root string) {
	rel, err := filepath.Rel(root, dir)
	prefix := ""
	if err == nil && rel != "." {
		prefix = filepath.ToSlash(rel) + "/"
	}
	for _, filename := range ignoreFileNames {
		data, err := os.ReadFile(filepath.Join(dir, filename))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if rule, ok := prefixIgnorePattern(line, prefix); ok {
				m.rules = append(m.rules, rule)
			}
		}
	}
}

// prefixIgnorePattern normalizes one ignore line and scopes it to its
// declaring directory (pi's prefixIgnorePattern).
func prefixIgnorePattern(line, prefix string) (ignoreRule, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return ignoreRule{}, false
	}
	if strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, `\#`) {
		return ignoreRule{}, false
	}
	rule := ignoreRule{}
	pattern := line
	if strings.HasPrefix(pattern, "!") {
		rule.negate = true
		pattern = pattern[1:]
	} else if strings.HasPrefix(pattern, `\!`) {
		pattern = pattern[1:]
	}
	pattern = strings.TrimPrefix(pattern, "/")
	rule.pattern = prefix + pattern
	return rule, true
}

// ignores reports whether relSlash (slash-separated, root-relative;
// directories carry a trailing slash) is ignored. Later rules win (git
// semantics, pi's ig.ignores).
func (m *ignoreMatcher) ignores(relSlash string) bool {
	ignored := false
	for _, r := range m.rules {
		if matchIgnore(r.pattern, relSlash) {
			ignored = !r.negate
		}
	}
	return ignored
}

// matchIgnore approximates the `ignore` package's gitignore matching via
// doublestar: trailing-slash patterns only match directory paths (which
// carry the trailing slash) and their contents; slash-free patterns match
// basenames at any depth.
func matchIgnore(pattern, relSlash string) bool {
	dirOnly := strings.HasSuffix(pattern, "/")
	pattern = strings.TrimSuffix(pattern, "/")
	if pattern == "" {
		return false
	}
	// Direct and subtree matches (a matched directory ignores its
	// contents, git's "build ignores build/**" rule).
	if !dirOnly {
		if ok, _ := doublestar.Match(pattern, relSlash); ok {
			return true
		}
	}
	if ok, _ := doublestar.Match(pattern+"/**", relSlash); ok {
		return true
	}
	// Slash-free patterns also match any basename (git's "*.log" rule)
	// and any same-named directory's subtree.
	if !strings.Contains(pattern, "/") {
		base := relSlash
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		if !dirOnly {
			if ok, _ := doublestar.Match(pattern, base); ok {
				return true
			}
		}
		if ok, _ := doublestar.Match("*/"+pattern+"/**", relSlash); ok {
			return true
		}
	}
	return false
}
