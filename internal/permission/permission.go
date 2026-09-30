// Package permission implements scode's permission engine: every tool
// call passes a single choke point that decides allow / ask / deny from
// declarative rules (design: docs/design-permission-plan-desktop.md).
//
// Rule syntax follows Claude Code: "tool" or "tool(pattern)", evaluated
// in deny → ask → allow order (first match wins per list); unmatched
// calls fall through to the mode default. The pattern domain depends on
// the tool: bash matches the command string, file tools match the path
// (cwd-relative, doublestar ** supported), other tools match by name
// only (tool-name globs cover MCP: "mcp__github__*").
//
// The engine is a pure evaluator: it never prompts and never touches
// the filesystem. Callers own the Approver interaction and persistence.
package permission

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"scode/internal/llm"
)

// Decision is the choke-point verdict for one tool call.
type Decision int

const (
	Allow Decision = iota
	Ask
	Deny
)

func (d Decision) String() string {
	switch d {
	case Ask:
		return "ask"
	case Deny:
		return "deny"
	default:
		return "allow"
	}
}

// Mode is the fallback for calls no rule matches.
type Mode string

const (
	// ModeDefault allows unmatched calls (status-quo behavior; rules
	// only ever restrict).
	ModeDefault Mode = "default"
	// ModeBypass allows everything, ignoring all rules (deliberate
	// override; deny rules included).
	ModeBypass Mode = "bypass"
)

// Rule is one parsed "tool(pattern)" entry.
type Rule struct {
	Raw        string // canonical form as written
	Tool       string // tool name, may carry a glob (mcp__server__*)
	Pattern    string // "" when name-only
	HasPattern bool
}

// ParseRule parses "tool" or "tool(pattern)". The tool name is required;
// the pattern may be empty (matches every call of that tool).
func ParseRule(s string) (Rule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Rule{}, fmt.Errorf("empty rule")
	}
	r := Rule{Raw: s}
	if i := strings.IndexByte(s, '('); i >= 0 {
		if !strings.HasSuffix(s, ")") {
			return Rule{}, fmt.Errorf("rule %q: missing closing paren", s)
		}
		r.Tool = strings.TrimSpace(s[:i])
		r.Pattern = s[i+1 : len(s)-1]
		r.HasPattern = true
	} else {
		r.Tool = s
	}
	if r.Tool == "" {
		return Rule{}, fmt.Errorf("rule %q: empty tool name", s)
	}
	return r, nil
}

// Engine evaluates tool calls against ordered rule lists. Lists are
// checked deny → ask → allow, first match in each list wins; a deny
// match short-circuits everything.
type Engine struct {
	CWD  string
	Mode Mode

	deny  []Rule
	ask   []Rule
	allow []Rule

	// defaults is a fallback tier evaluated AFTER the configured lists
	// (MCP server defaultPolicy rules live here): an explicit settings
	// rule always beats a server default, even across lists.
	defaults []DefaultRule

	sessionAllow map[string]bool // 'a' key: exact rules remembered for the session
}

// DefaultRule is a rule paired with its verdict (the fallback tier).
type DefaultRule struct {
	Rule
	Decision Decision
}

// AddDefault appends a fallback-tier rule (MCP server defaultPolicy).
func (e *Engine) AddDefault(raw string, d Decision) error {
	r, err := ParseRule(raw)
	if err != nil {
		return err
	}
	e.defaults = append(e.defaults, DefaultRule{Rule: r, Decision: d})
	return nil
}

// RemoveDefault drops every fallback-tier rule with the given raw
// pattern — the symmetric undo of AddDefault, so deleting or
// reconfiguring an MCP server clears (and can re-add) its
// mcp__<server>__* default without duplicates piling up.
func (e *Engine) RemoveDefault(raw string) {
	kept := e.defaults[:0]
	for _, d := range e.defaults {
		if d.Rule.Raw != raw {
			kept = append(kept, d)
		}
	}
	e.defaults = kept
}

// New builds an engine from raw rule strings (settings.json shape).
// Unknown modes fall back to ModeDefault.
func New(cwd, mode string, allow, ask, deny []string) (*Engine, error) {
	e := &Engine{CWD: cwd, Mode: Mode(mode), sessionAllow: map[string]bool{}}
	if e.Mode != ModeBypass {
		e.Mode = ModeDefault
	}
	for _, dst := range []struct {
		raws []string
		out  *[]Rule
	}{
		{deny, &e.deny}, {ask, &e.ask}, {allow, &e.allow},
	} {
		for _, raw := range dst.raws {
			r, err := ParseRule(raw)
			if err != nil {
				return nil, err
			}
			*dst.out = append(*dst.out, r)
		}
	}
	return e, nil
}

// AddAllow appends a runtime allow rule ('p' persistence also writes it
// to project settings; the engine update makes it live immediately).
func (e *Engine) AddAllow(raw string) error {
	r, err := ParseRule(raw)
	if err != nil {
		return err
	}
	e.allow = append(e.allow, r)
	return nil
}

// AllowForSession remembers an exact rule for the session ('a' key).
// Checked before all configured lists — a session grant is the most
// specific statement of intent.
func (e *Engine) AllowForSession(raw string) {
	e.sessionAllow[raw] = true
}

// Evaluate decides one call, returning the verdict and the matched rule
// (nil when the mode default decided).
//
// Bash chain semantics differ by tier: deny and ask rules fire when ANY
// command segment matches (one dangerous segment stops the chain), while
// an allow rule must cover EVERY segment (an allow for "npm test" never
// green-lights "npm test && anything-else").
func (e *Engine) Evaluate(call llm.Block) (Decision, *Rule) {
	if e.Mode == ModeBypass {
		return Allow, nil
	}
	if e.sessionAllow[ExactRule(e.CWD, call)] {
		return Allow, nil
	}
	if r := matchAny(e.deny, e.CWD, call, true); r != nil {
		return Deny, r
	}
	if r := matchAny(e.ask, e.CWD, call, true); r != nil {
		return Ask, r
	}
	if r := matchAny(e.allow, e.CWD, call, false); r != nil {
		return Allow, r
	}
	for i := range e.defaults {
		blocking := e.defaults[i].Decision != Allow
		if e.defaults[i].matches(e.CWD, call, blocking) {
			return e.defaults[i].Decision, &e.defaults[i].Rule
		}
	}
	return Allow, nil // ModeDefault: unrestricted
}

// matchAny returns the first rule matching the call, or nil. blocking
// selects the bash chain semantics (any-segment vs all-segments).
func matchAny(rules []Rule, cwd string, call llm.Block, blocking bool) *Rule {
	for i := range rules {
		if rules[i].matches(cwd, call, blocking) {
			return &rules[i]
		}
	}
	return nil
}

// Matches reports whether the rule covers this call under allow-tier
// semantics (every bash command segment must match).
func (r Rule) Matches(cwd string, call llm.Block) bool {
	return r.matches(cwd, call, false)
}

// MatchesBlocking reports whether the rule covers this call under
// deny/ask-tier semantics (any matching bash segment is enough).
func (r Rule) MatchesBlocking(cwd string, call llm.Block) bool {
	return r.matches(cwd, call, true)
}

func (r Rule) matches(cwd string, call llm.Block, blocking bool) bool {
	if !toolNameMatches(r.Tool, call.Name) {
		return false
	}
	if !r.HasPattern {
		return true
	}
	kind, value := matchTarget(call)
	switch kind {
	case "bash":
		return bashPatternMatches(r.Pattern, value, blocking)
	case "path":
		return pathPatternMatches(r.Pattern, value, cwd)
	default:
		// The rule restricts arguments this tool does not expose —
		// it can never match (name-only rules cover these tools).
		return false
	}
}

// toolNameMatches: exact, or doublestar glob (mcp__server__*).
func toolNameMatches(pattern, name string) bool {
	if pattern == name {
		return true
	}
	if strings.ContainsAny(pattern, "*?") {
		ok, _ := doublestar.Match(pattern, name)
		return ok
	}
	return false
}

// CallSummary exposes the match target for presentation (the approval
// prompt shows "command: …" for bash, "path: …" for file tools).
func CallSummary(call llm.Block) (kind, value string) {
	return matchTarget(call)
}

// MatchPathGlob exposes the rule path matcher for mode guards (plan
// mode's .scode/plan whitelist uses it).
func MatchPathGlob(pattern, argPath, cwd string) bool {
	return pathPatternMatches(pattern, argPath, cwd)
}

// matchTarget extracts the value a rule's pattern matches against:
// bash → the command string; file tools → the path argument.
func matchTarget(call llm.Block) (kind, value string) {
	var args struct {
		Command string `json:"command"`
		Path    string `json:"path"`
	}
	_ = json.Unmarshal(call.Arguments, &args) // malformed args fail at validation; matching is best-effort
	switch call.Name {
	case "bash":
		return "bash", args.Command
	case "write", "edit", "read", "ls", "find":
		return "path", args.Path
	default:
		return "", ""
	}
}

// bashPatternMatches follows Claude Code's prefix semantics, applied to
// command segments: the line is first split on shell separators (`;`,
// `&`, `|`, newlines). Blocking tiers (deny/ask) match when ANY segment
// matches — one dangerous segment stops the chain. The allow tier
// requires EVERY segment to match — otherwise bash(npm test) would
// green-light "npm test && arbitrary-command".
//
// Per segment:
//   - "*"                matches every command
//   - "git diff:*"       matches "git diff" with a word-boundary
//     continuation ("git diff HEAD" but not "git diffx")
//   - "git status"       matches exactly, or with a word-boundary
//     continuation ("git status -s" but not "git statuses")
func bashPatternMatches(pattern, command string, blocking bool) bool {
	if pattern == "*" {
		return true
	}
	segments := splitShellChain(command)
	if len(segments) == 0 {
		return false
	}
	for _, seg := range segments {
		if bashSegmentMatches(pattern, seg) == blocking {
			return blocking
		}
	}
	return !blocking
}

// bashSegmentMatches applies the prefix semantics to one command segment.
func bashSegmentMatches(pattern, command string) bool {
	if strings.HasSuffix(pattern, ":*") {
		prefix := strings.TrimSuffix(pattern, ":*")
		return command == prefix || strings.HasPrefix(command, prefix+" ")
	}
	return command == pattern || strings.HasPrefix(command, pattern+" ")
}

// splitShellChain splits a command line into its sequence/pipeline
// segments on shell separators (`;`, `&`, `|`, newlines — `&&` and `||`
// fall out of the single-character splits). Segments are whitespace-
// trimmed and empties (leading/trailing/doubled separators) dropped.
// This is a lexical split, not shell parsing: quoting and substitution
// are not honored, which errs toward MORE segments — conservative for
// matching, since every segment must satisfy the rule.
func splitShellChain(command string) []string {
	var out []string
	start := 0
	flush := func(end int) {
		if seg := strings.TrimSpace(command[start:end]); seg != "" {
			out = append(out, seg)
		}
	}
	for i := 0; i < len(command); i++ {
		switch command[i] {
		case ';', '&', '|', '\n':
			flush(i)
			start = i + 1
		}
	}
	flush(len(command))
	return out
}

// pathPatternMatches matches a doublestar glob against the call's path,
// tried both cwd-relative (slash form) and as-given, so rules stay
// portable (".scode/plan/**") while absolute patterns ("/tmp/**") work.
func pathPatternMatches(pattern, argPath, cwd string) bool {
	if pattern == "*" || pattern == "**" {
		return true
	}
	candidates := []string{filepath.ToSlash(argPath)}
	if cwd != "" && argPath != "" {
		abs := argPath
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(cwd, abs)
		}
		absSlash := filepath.ToSlash(filepath.Clean(abs))
		candidates = append(candidates, absSlash)
		if rel, err := filepath.Rel(cwd, abs); err == nil && !strings.HasPrefix(rel, "..") {
			candidates = append(candidates, filepath.ToSlash(rel))
		}
	}
	patSlash := filepath.ToSlash(pattern)
	for _, c := range candidates {
		if ok, _ := doublestar.Match(patSlash, c); ok {
			return true
		}
	}
	return false
}

// ExactRule renders the precise rule covering this call — the shape the
// 'a'/'p' approval keys remember. Deliberately NOT generalized
// (approving "git status" writes bash(git status), never bash(git:*)).
func ExactRule(cwd string, call llm.Block) string {
	kind, value := matchTarget(call)
	switch kind {
	case "bash":
		return fmt.Sprintf("bash(%s)", value)
	case "path":
		// Remember the cwd-relative form when possible: portable
		// across checkouts and matches the rules humans write.
		if cwd != "" && value != "" {
			abs := value
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(cwd, abs)
			}
			if rel, err := filepath.Rel(cwd, abs); err == nil && !strings.HasPrefix(rel, "..") {
				value = filepath.ToSlash(rel)
			}
		}
		return fmt.Sprintf("%s(%s)", call.Name, filepath.ToSlash(value))
	default:
		return call.Name
	}
}
