package tools

import (
	"bufio"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// doublestarMatch shells out to the doublestar glob engine (** support).
func doublestarMatch(pattern, name string) (bool, error) {
	return doublestar.Match(pattern, name)
}

// MatchGlob applies rg/fd basename semantics: a slash-free pattern such
// as "*.go" matches at ANY depth, not just the root (doublestar's "*"
// does not cross "/"). Patterns containing "/" stay full-path.
func MatchGlob(pattern, relSlash string) bool {
	if ok, _ := doublestar.Match(pattern, relSlash); ok {
		return true
	}
	if !strings.Contains(pattern, "/") {
		if ok, _ := doublestar.Match(pattern, path.Base(relSlash)); ok {
			return true
		}
	}
	return false
}

// ignoreMatcher applies .gitignore rules from the repo root: ordered
// patterns, "!" negation, trailing-slash directory scoping, and bare-name
// matching of everything beneath (git's "build ignores build/**" rule).
type ignoreMatcher struct {
	rules []ignoreRule
}

type ignoreRule struct {
	pattern string
	negate  bool
}

// LoadIgnoreMatcher parses root/.gitignore (if present). Nested
// .gitignore files arrive in a later revision; root-level covers the
// common cases for tool filtering.
func LoadIgnoreMatcher(root string) *ignoreMatcher {
	m := &ignoreMatcher{}
	f, err := os.Open(filepath.Join(root, ".gitignore"))
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r := ignoreRule{}
		if strings.HasPrefix(line, "!") {
			r.negate = true
			line = line[1:]
		}
		line = strings.TrimSuffix(line, "/")
		if line == "" {
			continue
		}
		r.pattern = line
		m.rules = append(m.rules, r)
	}
	return m
}

// Match reports whether relSlash (slash-separated, root-relative) is
// ignored. Later rules win, per git semantics.
func (m *ignoreMatcher) Match(relSlash string) bool {
	if m == nil {
		return false
	}
	ignored := false
	for _, r := range m.rules {
		if matchIgnorePattern(r.pattern, relSlash) {
			ignored = !r.negate
		}
	}
	return ignored
}

func matchIgnorePattern(pattern, relSlash string) bool {
	anchored := strings.HasPrefix(pattern, "/")
	pattern = strings.TrimPrefix(pattern, "/")
	if ok, _ := doublestar.Match(pattern, relSlash); ok {
		return true
	}
	// A bare directory name matches its contents: build -> build/anything.
	if ok, _ := doublestar.Match(pattern+"/**", relSlash); ok {
		return true
	}
	// Non-anchored, slash-free patterns also match any path's basename
	// (git's rule: "*.log" hits "src/build.log"); anchored patterns match
	// from the root only.
	if !anchored && !strings.Contains(pattern, "/") {
		base := filepath.Base(relSlash)
		if ok, _ := doublestar.Match(pattern, base); ok {
			return true
		}
	}
	return false
}
