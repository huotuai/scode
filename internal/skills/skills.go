// Package skills implements Agent Skills discovery and prompt injection
// (pi's core/skills.ts + the discovery conventions from
// core/package-manager.ts).
//
// A skill is a markdown file with YAML frontmatter (name, description,
// optional disable-model-invocation). SKILL.md marks a skill root. Skills
// are DISCOVERED, never eagerly loaded: the system prompt lists them in
// <available_skills>, and the model reads the file with the read tool
// when a task matches. Explicit invocation (/skill:name) inlines the
// file body into the user message as a <skill> block (pi's
// _expandSkillCommand).
package skills

import (
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Limits per the Agent Skills spec (pi's MAX_* constants).
const (
	MaxNameLength        = 64
	MaxDescriptionLength = 1024
)

// Skill is one discovered skill (pi's Skill, minus the rich SourceInfo —
// scode tracks the origin as a plain Source string).
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	FilePath    string `json:"filePath"`
	BaseDir     string `json:"baseDir"`
	// Source: "user" (~/.scode/skills, ~/.agents/skills), "project"
	// (.scode/skills, .agents/skills), or "path" (explicit paths).
	Source                 string `json:"source"`
	DisableModelInvocation bool   `json:"disableModelInvocation,omitempty"`
}

// Diagnostic is a non-fatal load finding (pi's ResourceDiagnostic).
// Warnings cover unreadable files and spec violations; collisions record
// which same-named skill won.
type Diagnostic struct {
	Type    string `json:"type"` // "warning" | "collision"
	Message string `json:"message"`
	Path    string `json:"path"`
	// Collision details (pi's collision.winnerPath/loserPath).
	WinnerPath string `json:"winnerPath,omitempty"`
	LoserPath  string `json:"loserPath,omitempty"`
}

// ValidateName checks the name against the Agent Skills spec (pi's
// validateName); returned strings are warning texts.
func ValidateName(name string) []string {
	var errors []string
	if len(name) > MaxNameLength {
		errors = append(errors, "name exceeds 64 characters")
	}
	if !nameCharsRe.MatchString(name) {
		errors = append(errors, "name contains invalid characters (must be lowercase a-z, 0-9, hyphens only)")
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		errors = append(errors, "name must not start or end with a hyphen")
	}
	if strings.Contains(name, "--") {
		errors = append(errors, "name must not contain consecutive hyphens")
	}
	return errors
}

// ValidateDescription checks the description (pi's validateDescription).
func ValidateDescription(description string) []string {
	if strings.TrimSpace(description) == "" {
		return []string{"description is required"}
	}
	if len(description) > MaxDescriptionLength {
		return []string{"description exceeds 1024 characters"}
	}
	return nil
}

// loadSkillFromFile parses one markdown file into a Skill (pi's
// loadSkillFromFile): SKILL.md files are "declared" skills (frontmatter
// parse failures are reported); bare .md files must carry a description
// to count at all. The name falls back to the parent directory name.
// Spec violations warn but still load; a missing/empty description skips.
func loadSkillFromFile(filePath, source string) (*Skill, []Diagnostic) {
	var diags []Diagnostic
	isDeclared := filepath.Base(filePath) == "SKILL.md"

	raw, err := os.ReadFile(filePath)
	if err != nil {
		diags = append(diags, Diagnostic{Type: "warning", Message: err.Error(), Path: filePath})
		return nil, diags
	}

	fm, _, err := ParseFrontmatter(string(raw))
	if err != nil {
		if isDeclared {
			diags = append(diags, Diagnostic{Type: "warning", Message: err.Error(), Path: filePath})
		}
		return nil, diags
	}

	description := fm.Description
	hasDescription := strings.TrimSpace(description) != ""
	if !isDeclared && !hasDescription {
		return nil, diags
	}

	skillDir := filepath.Dir(filePath)
	for _, e := range ValidateDescription(description) {
		diags = append(diags, Diagnostic{Type: "warning", Message: e, Path: filePath})
	}

	name := fm.Name
	if name == "" {
		name = filepath.Base(skillDir) // parent directory name (pi's fallback)
	}
	for _, e := range ValidateName(name) {
		diags = append(diags, Diagnostic{Type: "warning", Message: e, Path: filePath})
	}

	if !hasDescription {
		return nil, diags
	}

	return &Skill{
		Name:                   name,
		Description:            description,
		FilePath:               filePath,
		BaseDir:                skillDir,
		Source:                 source,
		DisableModelInvocation: fm.DisableModelInvocation,
	}, diags
}

// LoadResult bundles the outcome of a load (pi's LoadSkillsResult).
type LoadResult struct {
	Skills      []Skill
	Diagnostics []Diagnostic
}

// ---------------------------------------------------------------------------
// Prompt rendering (pi's formatSkillsForPrompt)
// ---------------------------------------------------------------------------

// FormatForPrompt renders the <available_skills> block for the system
// prompt. Skills with DisableModelInvocation are excluded (pi: they can
// only be invoked explicitly via /skill:name). Returns "" when no skills
// are visible. scode always has the read tool, so pi's fileReadTool is
// fixed to "read".
func FormatForPrompt(skills []Skill) string {
	var visible []Skill
	for _, s := range skills {
		if !s.DisableModelInvocation {
			visible = append(visible, s)
		}
	}
	if len(visible) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\nThe following skills provide specialized instructions for specific tasks.\n")
	b.WriteString("Use the read tool to load a skill's file when the task matches its description.\n")
	b.WriteString("When a skill file references a relative path, resolve it against the skill directory (parent of SKILL.md / dirname of the path) and use that absolute path in tool commands.\n")
	b.WriteString("\n<available_skills>\n")
	for _, s := range visible {
		b.WriteString("  <skill>\n")
		b.WriteString("    <name>" + escapeXML(s.Name) + "</name>\n")
		b.WriteString("    <description>" + escapeXML(s.Description) + "</description>\n")
		b.WriteString("    <location>" + escapeXML(s.FilePath) + "</location>\n")
		b.WriteString("  </skill>\n")
	}
	b.WriteString("</available_skills>")
	return b.String()
}

func escapeXML(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}

// ---------------------------------------------------------------------------
// Explicit invocation (pi's _expandSkillCommand + parseSkillBlock)
// ---------------------------------------------------------------------------

// ExpandCommand expands a "/skill:name args" input into the full skill
// block (pi's _expandSkillCommand): the frontmatter-stripped file body
// wrapped with name/location and the relative-reference note, followed
// by any args. Unknown commands and unknown skill names pass through
// unchanged (pi sends the original text as a normal prompt).
func ExpandCommand(text string, skills []Skill) string {
	if !strings.HasPrefix(text, "/skill:") {
		return text
	}
	rest := text[len("/skill:"):]
	name, args, _ := strings.Cut(rest, " ")
	args = strings.TrimSpace(args)

	var skill *Skill
	for i := range skills {
		if skills[i].Name == name {
			skill = &skills[i]
			break
		}
	}
	if skill == nil {
		return text // unknown skill: pass through (pi)
	}

	raw, err := os.ReadFile(skill.FilePath)
	if err != nil {
		return text // read failure: original text (pi emits an error event; scode surfaces nothing extra)
	}
	body := strings.TrimSpace(StripFrontmatter(string(raw)))
	block := `<skill name="` + skill.Name + `" location="` + skill.FilePath + `">` + "\n" +
		"References are relative to " + skill.BaseDir + ".\n\n" +
		body + "\n</skill>"
	if args != "" {
		return block + "\n\n" + args
	}
	return block
}

// ---------------------------------------------------------------------------
// Aggregation (pi's loadSkills + the resource-loader's discovery order)
// ---------------------------------------------------------------------------

// Options drives Load. Paths are classified user/project/path like pi's
// getSource when defaults are supplied separately.
type Options struct {
	CWD      string
	AgentDir string // ~/.scode (or SCODE_DIR)
	// CLIPaths are --skill flag values (files or directories).
	CLIPaths []string
	// SettingsPaths are settings.json skills entries (extra directories).
	SettingsPaths []string
	// NoSkills disables default discovery (pi's --no-skills); explicit
	// paths still load.
	NoSkills bool
}

// Load aggregates skills from every configured location (pi's discovery
// precedence, first-wins on name collision):
//
//  1. project: {cwd}/.scode/skills (pi mode)
//  2. project: {cwd}/.agents/skills and ancestors up to the git root
//     (agents mode, nearest first)
//  3. user: {agentDir}/skills (pi mode)
//  4. user: ~/.agents/skills (agents mode)
//  5. settings paths
//  6. CLI --skill paths
//
// pi's npm package resources, override patterns, and extension-contributed
// paths have no scode counterpart and are not ported.
func Load(opts Options) LoadResult {
	cwd, _ := filepath.Abs(opts.CWD)
	agentDir, _ := filepath.Abs(opts.AgentDir)

	var result LoadResult
	skillMap := map[string]int{} // name → index in result.Skills
	realPaths := map[string]bool{}
	var collisions []Diagnostic

	add := func(r LoadResult) {
		result.Diagnostics = append(result.Diagnostics, r.Diagnostics...)
		for _, s := range r.Skills {
			real := canonicalPath(s.FilePath)
			if realPaths[real] {
				continue // same file via symlink/duplicate path (pi)
			}
			if winner, ok := skillMap[s.Name]; ok {
				collisions = append(collisions, Diagnostic{
					Type:       "collision",
					Message:    `name "` + s.Name + `" collision`,
					Path:       s.FilePath,
					WinnerPath: result.Skills[winner].FilePath,
					LoserPath:  s.FilePath,
				})
				continue
			}
			skillMap[s.Name] = len(result.Skills)
			result.Skills = append(result.Skills, s)
			realPaths[real] = true
		}
	}

	if !opts.NoSkills {
		// 1-2. Project locations.
		add(LoadFromDir(filepath.Join(cwd, ".scode", "skills"), "project", ModePi))
		homeAgents := filepath.Join(homeDir(), ".agents", "skills")
		for _, dir := range ancestorAgentsSkillDirs(cwd) {
			if canonicalPath(dir) == canonicalPath(homeAgents) {
				continue // ~/.agents/skills loads as a USER resource below (pi)
			}
			add(LoadFromDir(dir, "project", ModeAgents))
		}
		// 3-4. User locations.
		add(LoadFromDir(filepath.Join(agentDir, "skills"), "user", ModePi))
		add(LoadFromDir(homeAgents, "user", ModeAgents))
	}

	// 5-6. Explicit paths (settings, then CLI — pi's CLI paths land last).
	classify := func(p string) string {
		if isUnderPath(p, filepath.Join(agentDir, "skills")) {
			return "user"
		}
		if isUnderPath(p, filepath.Join(cwd, ".scode", "skills")) {
			return "project"
		}
		return "path"
	}
	for _, raw := range append(append([]string{}, opts.SettingsPaths...), opts.CLIPaths...) {
		p := resolvePath(raw, cwd)
		info, err := os.Stat(p)
		if err != nil {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Type: "warning", Message: "skill path does not exist", Path: p})
			continue
		}
		source := classify(p)
		if info.IsDir() {
			add(LoadFromDir(p, source, ModePi))
		} else if strings.HasSuffix(p, ".md") {
			skill, diags := loadSkillFromFile(p, source)
			result.Diagnostics = append(result.Diagnostics, diags...)
			if skill != nil {
				add(LoadResult{Skills: []Skill{*skill}})
			}
		} else {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Type: "warning", Message: "skill path is not a markdown file", Path: p})
		}
	}

	result.Diagnostics = append(result.Diagnostics, collisions...)
	return result
}

// ancestorAgentsSkillDirs lists {dir}/.agents/skills from cwd up to and
// including the git root (pi's collectAncestorAgentsSkillDirs), nearest
// first. Without a git root the walk runs to the filesystem root.
func ancestorAgentsSkillDirs(cwd string) []string {
	var dirs []string
	gitRoot := findGitRoot(cwd)
	dir := filepath.Clean(cwd)
	for {
		dirs = append(dirs, filepath.Join(dir, ".agents", "skills"))
		if gitRoot != "" && canonicalPath(dir) == canonicalPath(gitRoot) {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return dirs
}

func findGitRoot(start string) string {
	dir := filepath.Clean(start)
	for {
		if st, err := os.Stat(filepath.Join(dir, ".git")); err == nil && st.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func isUnderPath(target, root string) bool {
	target = canonicalPath(target)
	root = canonicalPath(root)
	return target == root || strings.HasPrefix(target, root+string(filepath.Separator))
}

// resolvePath expands ~ and makes p absolute relative to base (pi's
// resolvePath with trim).
func resolvePath(p, base string) string {
	p = strings.TrimSpace(p)
	if p == "~" {
		return homeDir()
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		return filepath.Join(homeDir(), p[2:])
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

// canonicalPath normalizes for dedup: absolute + symlink-resolved when
// possible (pi's canonicalizePath).
func canonicalPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return abs
}

// ---------------------------------------------------------------------------
// Change detection (hot reload)
// ---------------------------------------------------------------------------

// Fingerprint hashes every directory and .md file under the roots Load
// would scan (path + mtime + size) into one cheap change-detection token.
// Callers snapshot it after a Load and recompute it later: a mismatch
// means re-running Load may yield a different skill set. Non-.md files
// never influence discovery, so they are skipped; ignore rules are
// irrelevant here — over-triggering a reload is harmless.
func Fingerprint(opts Options) uint64 {
	h := fnv.New64a()
	cwd, _ := filepath.Abs(opts.CWD)
	agentDir, _ := filepath.Abs(opts.AgentDir)

	var roots []string
	if !opts.NoSkills {
		roots = append(roots, filepath.Join(cwd, ".scode", "skills"))
		homeAgents := filepath.Join(homeDir(), ".agents", "skills")
		for _, dir := range ancestorAgentsSkillDirs(cwd) {
			if canonicalPath(dir) == canonicalPath(homeAgents) {
				continue // user resource, appended below (Load's order)
			}
			roots = append(roots, dir)
		}
		roots = append(roots, filepath.Join(agentDir, "skills"), homeAgents)
	}
	for _, raw := range append(append([]string{}, opts.SettingsPaths...), opts.CLIPaths...) {
		roots = append(roots, resolvePath(raw, cwd))
	}

	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			continue // missing roots yield nothing (Load's tolerance)
		}
		if !info.IsDir() {
			if strings.HasSuffix(root, ".md") {
				hashEntry(h, root, info)
			}
			continue
		}
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error { //nolint:errcheck
			if err != nil {
				return nil // unreadable entries: Load tolerates them too
			}
			if !d.IsDir() && !strings.HasSuffix(d.Name(), ".md") {
				return nil
			}
			if info, ierr := d.Info(); ierr == nil {
				hashEntry(h, p, info)
			}
			return nil
		})
	}
	return h.Sum64()
}

// hashEntry folds one filesystem entry into the fingerprint. Files
// hash by path+mtime+size; DIRECTORIES hash by path only: Windows
// propagates directory mtime updates lazily after child writes, so a
// freshly created tree can fingerprint differently microseconds apart,
// and a non-.md sibling write would otherwise leak into a fingerprint
// whose discovery contract declares it invisible.
func hashEntry(h interface{ Write([]byte) (int, error) }, p string, info fs.FileInfo) {
	if info.IsDir() {
		fmt.Fprintf(h, "%s\n", p) //nolint:errcheck
		return
	}
	fmt.Fprintf(h, "%s|%d|%d\n", p, info.ModTime().UnixNano(), info.Size()) //nolint:errcheck
}

// nameCharsRe is pi's skill name character class.
var nameCharsRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// homeDir is the user's home (pi's homedir()).
func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}
