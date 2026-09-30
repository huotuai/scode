// Package cli wires config, prompt, agent, and session persistence into
// the user-facing entry points (print mode and REPL).
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"scode/internal/agent"
	"scode/internal/checkpoint"
	"scode/internal/config"
	"scode/internal/i18n"
	"scode/internal/llm"
	"scode/internal/mcp"
	"scode/internal/memory"
	"scode/internal/permission"
	"scode/internal/planmode"
	"scode/internal/plantrack"
	"scode/internal/prompt"
	"scode/internal/sandbox"
	"scode/internal/session"
	"scode/internal/skills"
	"scode/internal/subagent"
	"scode/internal/tools"
)

// App is one running scode instance bound to a working directory.
//
// Storage discipline (pi's boundary): the session file carries the
// CONVERSATION only (messages + compaction markers); a.entries mirrors
// it. The system prompt and tool declarations are rebuilt fresh at
// load — they live only in the in-memory transcript's leading system
// message (a.sysMsg), never on disk, so prompt/tool drift needs no
// reconciliation and compaction has no system deltas to lose.
type App struct {
	CWD      string
	CfgDir   string
	Provider llm.Provider
	Model    llm.Model
	Settings *config.Settings

	// thinkingLevel is the session's reasoning effort ("" | off | low |
	// medium | high); mirrored into the agent's stream options so live
	// switches apply to the next run (SetThinkingLevel).
	thinkingLevel string

	Agent         *agent.Agent
	Store         *session.Store
	Sess          *session.Session
	Tr            *llm.Transcript
	sysMsg        llm.Message // freshly built leading system message (request-time only)
	entries       []session.Entry
	persisted     int // transcript messages durably settled (write watermark)
	fileOps       session.FileOps
	compactTokens int

	pricing llm.Pricing
	spent   llm.Usage // session-wide accumulated usage

	skills []skills.Skill // loaded at setup; /skill:name expands against these (pi)
	// skillOpts/skillFp enable hot reload: the discovery config is kept
	// so /reload (or the prompt-boundary fingerprint check) can re-run
	// skills.Load mid-session without a restart.
	skillOpts skills.Options
	skillFp   uint64
	skillsMu  sync.RWMutex // guards skills + skillFp (steer reads mid-run)

	mcpManager   *mcp.Manager      // nil when mcp.json has no servers
	mcpRegistry  *agent.Registry   // the live registry (MCP generations swap through it)
	mcpInstructs map[string]string // server name → current instructions text
	mcpMu        sync.Mutex        // serializes MCP hook mutations of registry + transcript
	mcpHooks     mcp.Hooks         // kept for the lazy manager creation (runtime /mcp adds)

	agentsPath string     // agents.json (sub-agent definitions, RMW)
	spendMu    sync.Mutex // guards spent: parallel sub-agents fold usage concurrently

	memStore    *memory.Store // long-term memory (memory/<safe-cwd>.json)
	interactive bool          // an interactive surface (REPL/TUI): gates close-time auto-extraction

	checkpoints *checkpoint.Store // code-rewind points (edit/write before-states)

	perms    *permission.Engine // tool-call choke point (design: docs/design-permission-plan-desktop.md)
	approver *Approver          // resolves ask verdicts; interactive in the REPL, auto-deny in print mode
	planCtl  *planmode.Controller
	planTrk  *plantrack.Tracker // the update_plan checklist (persisted as plan entries)

	// sandboxDefault is the deployment default from settings; the
	// session override folds from sandbox entries on the log.
	sandboxDefault  sandbox.Mode
	sandboxOverride sandbox.Mode

	// persistMu serializes session-file writes: mode entries land from
	// tool goroutines (plan approval mid-run) while the drive loop
	// persists messages — the bufio writer is not concurrency-safe.
	persistMu sync.Mutex
	closed    bool // set under persistMu once the session file is closed

	// autoTitle enables background LLM session-title generation
	// (SCODE_AUTO_TITLE; the desktop sets it when spawning `scode serve`).
	autoTitle bool

	// titleGen guards session-title generation: 0 idle, 1 in flight,
	// 2 done. A failure resets it to idle so a later turn retries.
	titleGen atomic.Int32
}

// Options select provider/model/session at startup.
type Options struct {
	Provider      string
	Model         string
	Resume        string   // session id
	ThinkingLevel string   // off | low | medium | high
	SkillPaths    []string // --skill flag values (files or directories)
	NoSkills      bool     // --no-skills: disable default skill discovery
	Plan          bool     // --plan: start the session in plan mode
	GenerateTitle bool     // background LLM session titles (serve)
	// Approver overrides the terminal approval seam (the serve
	// transport injects its JSON-RPC implementation).
	Approver AskReviewer
}

// Setup resolves configuration and builds the app. With Resume set, the
// previous transcript continues (cache prefix intact when unchanged).
func Setup(opts Options) (*App, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	cfgDir, err := config.EnsureDir()
	if err != nil {
		return nil, err
	}
	settings, err := config.LoadSettings()
	if err != nil {
		return nil, err
	}
	p, modelID, providerName, err := config.ResolveProvider(settings, opts.Provider, opts.Model)
	if err != nil {
		return nil, err
	}

	a := &App{
		CWD:       cwd,
		CfgDir:    cfgDir,
		Provider:  p,
		Model:     buildModel(settings, providerName, modelID),
		Settings:  settings,
		autoTitle: opts.GenerateTitle,
	}
	a.compactTokens = resolveCompactTokens(settings, a.Model.ContextWindow)
	// /config: auto-compaction off wins over any threshold (negative
	// disables the trigger in NeedsCompaction).
	if !settings.AutoCompactOn() {
		a.compactTokens = -1
	}
	if p := settings.Providers[providerName].Pricing; p != nil {
		a.pricing = *p
	}
	// Sandbox default: absent/empty means workspace-write (the shipped
	// posture, dsh's default preset: write inside the workspace and
	// permitted temp areas, wider retries need approval); an invalid mode
	// fails LOUD at setup (dsh: config misspellings error explicitly,
	// never silently shift policy).
	a.sandboxDefault = sandbox.ModeWorkspaceWrite
	if settings.Sandbox != nil && settings.Sandbox.Mode != "" {
		m, err := sandbox.ParseMode(settings.Sandbox.Mode)
		if err != nil {
			return nil, fmt.Errorf("sandbox: %w", err)
		}
		a.sandboxDefault = m
	}
	// Confined by default needs a working backend: say so once at setup
	// rather than failing every confined call with SANDBOX_UNAVAILABLE. The
	// probe is cached, so this costs at most one spawn per process.
	if a.sandboxDefault != sandbox.ModeDangerFullAccess {
		if err := sandbox.Available(); err != nil {
			fmt.Fprintf(os.Stderr, "sandbox: mode %s is configured but no backend is usable here: %v\n", a.sandboxDefault, err)
		}
	}

	// Permission engine: user settings + project settings (.scode/
	// settings.json lists append) + MCP server defaultPolicy as the
	// fallback tier. The approver starts non-interactive (print mode);
	// the REPL flips it interactive after Setup.
	proj, err := config.LoadProjectSettings(cwd)
	if err != nil {
		return nil, err
	}
	engine, err := buildPermissionEngine(cwd, settings, proj)
	if err != nil {
		return nil, fmt.Errorf("permissions: %w", err)
	}
	a.perms = engine
	a.approver = NewApprover(false, os.Stdout, os.Stderr)
	a.planCtl = &planmode.Controller{}
	a.planTrk = &plantrack.Tracker{}
	// The approval seam: an injected AskReviewer (serve transport) wins;
	// otherwise the terminal approver.
	var reviewer AskReviewer = a.approver
	if opts.Approver != nil {
		reviewer = opts.Approver
	}

	store, err := session.NewStore(session.DefaultRoot(cfgDir, cwd))
	if err != nil {
		return nil, err
	}
	a.Store = store

	// The registry feeds both the prompt's tool section and the tool
	// declarations in the leading system message — providers read them
	// from the transcript, so they must be attached here.
	registry := tools.NewCodingRegistry()
	// exit_plan_mode is permanently registered (dsh: entering or leaving
	// plan mode changes only the prompt section, not the tool catalog).
	registry.Add(planmode.NewExitTool(a.planCtl, reviewer))
	// update_plan is always available too: the plan checklist lives for
	// the session's whole life, in and out of plan mode.
	registry.Add(plantrack.NewUpdateTool(a.planTrk))
	// The sub-agent delegation tool. Definitions live in agents.json
	// and are resolved PER CALL (fresh provider, fresh read-only
	// registry, fresh transcript), so /agents edits apply mid-session
	// and parallel delegates share nothing — not even with the parent.
	a.agentsPath = filepath.Join(cfgDir, "agents.json")
	registry.Add(subagent.NewTaskTool(subagent.Host{
		Specs: func() ([]subagent.Spec, error) { return subagent.Load(a.agentsPath) },
		Resolve: func(model string) (llm.Provider, llm.Model, string, error) {
			return a.resolveSubAgentModel(model)
		},
		Env: func() map[string]string {
			// Read at call time: the session and standing model are
			// settled by the time any delegate runs.
			return config.ShellEnv(a.Sess.Header().ID, a.Model.Provider, a.Model.ID)
		},
		CWD: func() string { return cwd },
		Sandbox: func() *agent.SandboxPolicy {
			pol := a.SandboxPolicy()
			return &agent.SandboxPolicy{Mode: string(pol.Mode), WorkspaceRoot: pol.WorkspaceRoot}
		},
		Spend: a.addSpend,
	}))
	a.mcpRegistry = registry
	// Code rewind: snapshot every edit/write before-state (the
	// /config rewindCheckpoints gate; declarations delegate unchanged).
	a.checkpoints = checkpoint.NewStore(cfgDir)
	if settings.RewindCheckpointsOn() {
		a.wrapCheckpoints(registry)
	}
	// MCP servers connect at setup like dsh's plugin activation: initial
	// attempts settle in parallel, discovered tools join the initial
	// declaration, instructions join the prompt. Startup failures are
	// tolerated per-server (failOnStartupError flips one to fatal);
	// the reconnect supervisor keeps trying in the background.
	mcpFile, err := mcp.LoadFile(filepath.Join(cfgDir, "mcp.json"))
	if err != nil {
		return nil, err
	}
	// MCP server defaultPolicy rules join the engine's fallback tier
	// (explicit settings rules beat them even across lists).
	for name, srv := range mcpFile.MCPServers {
		policy, err := mcpDefaultPolicy(name, srv.DefaultPolicy)
		if err != nil {
			return nil, err
		}
		if err := a.perms.AddDefault("mcp__"+name+"__*", policy); err != nil {
			return nil, err
		}
	}
	// Hooks stay on the App: runtime MCP configuration (/mcp) reuses
	// them when lazily creating the manager for the first added server.
	a.mcpHooks = mcp.Hooks{
		ToolsChanged:        a.onMCPToolsChanged,
		InstructionsChanged: a.onMCPInstructionsChanged,
		Logf: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		},
	}
	if len(mcpFile.MCPServers) > 0 {
		a.mcpInstructs = map[string]string{}
		mgr, err := a.newMCPManager(mcpFile)
		if err != nil {
			return nil, err
		}
		a.mcpManager = mgr
		mgr.Start()
		ready := mgr.WaitReady()
		if err := mgr.FatalStartupError(ready); err != nil {
			mgr.Shutdown()
			return nil, err
		}
		for server, err := range ready {
			if err != nil {
				fmt.Fprintf(os.Stderr, "mcp(%s): startup failed, reconnect loop active: %v\n", server, err)
			}
		}
		for _, t := range mgr.Tools() {
			registry.Add(t)
		}
		a.mcpInstructs = mgr.Instructions()
	}
	// Skill discovery runs at setup like pi's resource loader: the
	// listing joins the system prompt, and /skill:name expansion reads
	// the same set. Diagnostics surface on stderr (pi's loaded-resources
	// warnings). The options are kept for hot reload (/reload).
	a.skillOpts = skills.Options{
		CWD:           cwd,
		AgentDir:      cfgDir,
		CLIPaths:      opts.SkillPaths,
		SettingsPaths: settings.Skills,
		NoSkills:      opts.NoSkills,
	}
	skillsResult := skills.Load(a.skillOpts)
	for _, d := range skillsResult.Diagnostics {
		fmt.Fprintf(os.Stderr, "skills: %s: %s (%s)\n", d.Type, d.Message, d.Path)
	}
	a.skills = skillsResult.Skills
	a.skillFp = skills.Fingerprint(a.skillOpts)
	sysMsg := prompt.Build(cfgDir, cwd, registry.Contributions(), a.skills)
	// Long-term memory: when autoMemory is on, the (relevance-filtered)
	// entries join as a "memory" section before cwd — the same
	// insert-before-cwd discipline as MCP instructions.
	a.memStore = memory.Open(cfgDir, cwd)
	if text := a.memorySection(); text != "" {
		for i, s := range sysMsg.Sections {
			if s.Name == "cwd" {
				sysMsg.Sections = append(sysMsg.Sections[:i], append([]llm.Section{{Name: "memory", Value: text}}, sysMsg.Sections[i:]...)...)
				break
			}
		}
	}
	// MCP server instructions land in their own section, after skills
	// and before cwd (dsh's MCP_SERVERS order slot).
	if text := mcpInstructionsText(a.mcpInstructs); text != "" {
		inserted := false
		for i, s := range sysMsg.Sections {
			if s.Name == "cwd" {
				sysMsg.Sections = append(sysMsg.Sections[:i], append([]llm.Section{{Name: "mcp", Value: text}}, sysMsg.Sections[i:]...)...)
				inserted = true
				break
			}
		}
		if !inserted {
			sysMsg.Sections = append(sysMsg.Sections, llm.Section{Name: "mcp", Value: text})
		}
	}
	sysMsg.ToolsAdded = registry.Decls()

	if opts.Resume != "" {
		rec, err := store.Load(opts.Resume)
		if err != nil {
			return nil, fmt.Errorf("resume %s: %w", opts.Resume, err)
		}
		a.entries = rec.Path()
		a.fileOps = rec.LatestFileOps()
		// Continue appending to the ORIGINAL file: the id stays
		// resumable forever and a.entries mirrors what is on disk.
		sess, err := store.OpenForAppend(rec.Header.ID)
		if err != nil {
			return nil, err
		}
		a.Sess = sess
		// The prompt and tool loadout come from the CURRENT build —
		// rebuilt, never reconciled against storage (pi rebuilds the
		// prompt at load): AGENTS.md edits and tool schema changes apply
		// with no delta messages and nothing stored to reconcile.
		tr, err := llm.NewTranscript(sysMsg, session.Project(a.entries)...)
		if err != nil {
			sess.Close() //nolint:errcheck
			return nil, err
		}
		a.Tr = tr
		a.persisted = tr.Len() // conversation is on disk; the leading system message is request-time only
		// Session-cumulative usage survives resume: every historical
		// assistant usage is on disk (stamped with cost at persist
		// time) and syncTranscript will never re-see it — fold it here
		// so the cache-hit rate and spend keep their session-wide
		// totals instead of restarting at zero. a.entries (the full
		// root→leaf path, pre-compaction history included) matches what
		// syncTranscript accumulated live; the projected transcript
		// alone would drop everything before the last compaction.
		for _, e := range a.entries {
			if e.Msg == nil {
				continue
			}
			if u := session.ValidUsage(*e.Msg); u != nil {
				a.spent = a.spent.Add(*u)
			}
		}
	} else {
		tr, err := llm.NewTranscript(sysMsg)
		if err != nil {
			return nil, err
		}
		a.Tr = tr
		sess, err := store.Create("", cwd)
		if err != nil {
			return nil, err
		}
		a.Sess = sess
		a.persisted = 0 // the leading system message skips storage on the first sync
	}
	a.sysMsg = sysMsg

	// Sandbox-mode replay (dsh sandbox/mode, translated): the session
	// override folds from the log; a confined mode lands as a section
	// delta so the model sees the standing policy (request-time only).
	if sm := session.CurrentSandbox(a.entries); sm != "" {
		if m, err := sandbox.ParseMode(sm); err == nil {
			a.sandboxOverride = m
		}
	}
	if pol := a.SandboxPolicy(); sandbox.PromptSection(pol) != "" {
		if err := a.Tr.Append(sandboxDelta(pol)); err != nil {
			fmt.Fprintf(os.Stderr, "sandbox section: %v\n", err)
		}
	}

	// Model-state replay (pi's model_change): a switch recorded in the
	// log wins on resume unless the caller passed an explicit
	// --provider/--model.
	if opts.Provider == "" && opts.Model == "" {
		if mp, mm := session.CurrentModel(a.entries); mp != "" || mm != "" {
			np, nModel, nName, rerr := config.ResolveProvider(settings, mp, mm)
			if rerr != nil {
				fmt.Fprintf(os.Stderr, "model restore: %v\n", rerr)
			} else {
				p, modelID, providerName = np, nModel, nName
				a.Provider = p
				a.Model = buildModel(settings, providerName, modelID)
				a.compactTokens = resolveCompactTokens(settings, a.Model.ContextWindow)
				a.pricing = llm.Pricing{}
				if pc := settings.Providers[providerName].Pricing; pc != nil {
					a.pricing = *pc
				}
			}
		}
	}

	// Thinking-state replay: an effort switch recorded in the log wins
	// on resume unless the caller passed an explicit flag.
	a.thinkingLevel = resolveThinking(settings, opts.ThinkingLevel)
	if opts.ThinkingLevel == "" {
		if lv := session.CurrentThinking(a.entries); lv != "" {
			a.thinkingLevel = lv
		}
	}

	// Mode-state wiring (dsh plan/mode, translated): a transition
	// persists an append-only mode entry and lands as a transcript
	// section delta — the prefix stays byte-stable either way.
	a.planCtl.OnChange = a.applyPlanMode
	if session.CurrentMode(a.entries) == "plan" {
		// Resume: the state replays from the log; re-add the guidance
		// delta (request-time only, like the leading system message).
		a.planCtl.Restore(true)
		if err := a.Tr.Append(planDelta(true)); err != nil {
			fmt.Fprintf(os.Stderr, "plan mode restore: %v\n", err)
		}
	}
	// Plan progress replays from the log the same way (whole-value
	// replace, last wins); nothing request-side needs rebuilding.
	a.planTrk.OnChange = a.applyPlan
	if p := session.CurrentPlan(a.entries); p != nil {
		a.planTrk.Restore(*p)
	}
	if opts.Plan {
		a.planCtl.Set(true)
	}

	// /config: session-log retention — prune stale session files on
	// launch (the live session is never a candidate).
	if days := settings.LogRetentionDays; days > 0 {
		if n := store.PruneOlderThan(days, a.Sess.Path); n > 0 {
			fmt.Fprintf(os.Stderr, "%s", i18n.Tf("cli.run.pruned", n, days))
		}
	}

	agentCfg := agent.Config{
		Provider: p,
		Model:    a.Model,
		Stream: llm.StreamOptions{
			PromptCacheKey: a.Sess.Header().ID,
			ThinkingLevel:  a.thinkingLevel,
		},
		Tools:  registry,
		Retry:  config.ResolveRetry(settings),
		Env:    config.ShellEnv(a.Sess.Header().ID, providerName, modelID),
		CWD:    cwd,
		Before: beforeHook(engine, reviewer, cwd, a.planCtl),
		Sandbox: func() *agent.SandboxPolicy {
			pol := a.SandboxPolicy()
			return &agent.SandboxPolicy{Mode: string(pol.Mode), WorkspaceRoot: pol.WorkspaceRoot}
		},
		SandboxEscalation: func(req agent.EscalationRequest) agent.EscalationResult {
			res := reviewer.ReviewSandboxEscalation(req.Ctx, req)
			// "本会话生效": widen the standing session mode too (the
			// transition persists as a sandbox entry, replayable).
			if res.Approved && res.ApplyToSession {
				if err := a.SetSandboxMode(req.RequestedMode); err != nil {
					fmt.Fprintf(os.Stderr, "sandbox escalation persist: %v\n", err)
				}
			}
			return res
		},
	}
	a.Agent = agent.New(agentCfg)
	return a, nil
}

// buildPermissionEngine merges user-level and project-level
// permissions (project lists append after user lists) into the engine.
func buildPermissionEngine(cwd string, user, proj *config.Settings) (*permission.Engine, error) {
	var mode string
	var allow, ask, deny []string
	if p := user.Permissions; p != nil {
		mode = p.DefaultMode
		allow, ask, deny = p.Allow, p.Ask, p.Deny
	}
	if p := proj.Permissions; p != nil {
		if p.DefaultMode != "" {
			mode = p.DefaultMode
		}
		allow = append(allow, p.Allow...)
		ask = append(ask, p.Ask...)
		deny = append(deny, p.Deny...)
	}
	return permission.New(cwd, mode, allow, ask, deny)
}

// applyPlanMode persists a mode transition (append-only entry) and
// appends the guidance section delta (request-time only; the mode entry
// is the durable state).
func (a *App) applyPlanMode(active bool) {
	mode := "default"
	if active {
		mode = "plan"
	}
	if err := a.persist(session.Entry{Mode: &session.ModeEntry{Mode: mode}}); err != nil {
		fmt.Fprintf(os.Stderr, "plan mode persist: %v\n", err)
	}
	if a.Tr != nil {
		if err := a.Tr.Append(planDelta(active)); err != nil {
			fmt.Fprintf(os.Stderr, "plan mode delta: %v\n", err)
		}
	}
}

// applyPlan persists a plan-progress update (append-only plan entry,
// whole-value replace — the tracker already holds the state; the entry
// is the durable form replayed on resume/fork).
func (a *App) applyPlan(plan session.PlanEntry) {
	if err := a.persist(session.Entry{Plan: &plan}); err != nil {
		fmt.Fprintf(os.Stderr, "plan persist: %v\n", err)
	}
}

// PlanState returns the current plan checklist; ok is false when the
// session has no plan yet (session/plan renders the empty state then).
func (a *App) PlanState() (session.PlanEntry, bool) {
	return a.planTrk.Get()
}

// addSpend folds usage into the session accounting. Parallel sub-agent
// runs report concurrently, so the sum is mutex-guarded.
func (a *App) addSpend(u llm.Usage) {
	a.spendMu.Lock()
	a.spent = a.spent.Add(u)
	a.spendMu.Unlock()
}

// resolveSubAgentModel builds the provider for a sub-agent's model
// spec: "" inherits the session's standing model, "provider:model"
// resolves the pair, and a bare name resolves as a profile.
func (a *App) resolveSubAgentModel(model string) (llm.Provider, llm.Model, string, error) {
	if model == "" {
		return a.Provider, a.Model, a.Model.ID, nil
	}
	settings, err := config.LoadSettings()
	if err != nil {
		return nil, llm.Model{}, "", err
	}
	name, id := model, ""
	if prov, mid, ok := strings.Cut(model, ":"); ok && prov != "" && mid != "" {
		name, id = prov, mid
	}
	p, modelID, provName, err := config.ResolveProvider(settings, name, id)
	if err != nil {
		return nil, llm.Model{}, "", err
	}
	return p, buildModel(settings, provName, modelID), modelID, nil
}

// Agents returns the user's sub-agent definitions (nil when none
// configured) — the editable set in agents.json.
func (a *App) Agents() ([]subagent.Spec, error) {
	return subagent.Load(a.agentsPath)
}

// AllAgents returns the effective delegation surface: shipped
// built-ins overlaid by the user's agents.json entries.
func (a *App) AllAgents() []subagent.Spec {
	user, err := subagent.Load(a.agentsPath)
	if err != nil {
		return subagent.All(nil)
	}
	return subagent.All(user)
}

// AgentSave validates and persists one sub-agent definition.
func (a *App) AgentSave(s subagent.Spec) error {
	return subagent.Upsert(a.agentsPath, s)
}

// AgentDelete removes one sub-agent definition.
func (a *App) AgentDelete(name string) error {
	return subagent.Delete(a.agentsPath, name)
}

// AgentListText renders the /agents listing (REPL surface): built-ins
// included, un-overridden ones marked.
func (a *App) AgentListText() string {
	specs, err := a.Agents()
	if err != nil {
		return "error: " + err.Error()
	}
	merged := subagent.All(specs)
	userNames := map[string]bool{}
	for _, s := range specs {
		userNames[s.Name] = true
	}
	rows := make([]string, 0, len(merged))
	for _, s := range merged {
		model := s.Model
		if model == "" {
			model = i18n.T("tui.agents.inheritModel")
		}
		effort := s.Effort
		if effort == "" {
			effort = i18n.T("tui.agents.effortDefault")
		}
		name := s.Name
		if subagent.IsBuiltin(s.Name) && !userNames[s.Name] {
			name += i18n.T("cli.run.builtinMark")
		}
		row := i18n.Tf("cli.run.agentRow", name, model, effort)
		if s.Description != "" {
			row += " · " + s.Description
		}
		rows = append(rows, row)
	}
	return i18n.Tf("cli.run.agentsList", len(merged), strings.Join(rows, "\n"))
}

// TUIConfig is the /config panel snapshot the overlay edits.
type TUIConfig struct {
	AutoCompact       bool
	LogRetentionDays  int
	AutoMemory        bool
	TypedMemory       bool
	MemoryRelevance   bool
	MemoryAutoExtract bool
	RewindCheckpoints bool
	ClipboardWatch    bool
	Language          string // "" = system (auto-detect)
}

// TUIConfig reads the panel state from the freshest settings (falls
// back to the setup snapshot on a read failure).
func (a *App) TUIConfig() TUIConfig {
	s, err := config.LoadSettings()
	if err != nil {
		s = a.Settings
	}
	if s == nil {
		s = &config.Settings{}
	}
	return TUIConfig{
		AutoCompact:       s.AutoCompactOn(),
		LogRetentionDays:  s.LogRetentionDays,
		AutoMemory:        s.AutoMemoryOn(),
		TypedMemory:       s.TypedMemoryOn(),
		MemoryRelevance:   s.MemoryRelevanceOn(),
		MemoryAutoExtract: s.MemoryAutoExtractOn(),
		RewindCheckpoints: s.RewindCheckpointsOn(),
		ClipboardWatch:    s.ClipboardWatch == nil || *s.ClipboardWatch,
		Language:          s.Language,
	}
}

// SetAutoCompact persists the toggle and applies it live: off parks the
// compaction threshold at -1 (NeedsCompaction's disabled sentinel);
// on re-resolves the threshold from the freshest settings.
func (a *App) SetAutoCompact(on bool) error {
	if err := config.SetTUIConfigKey("autoCompact", on); err != nil {
		return err
	}
	if on {
		if s, err := config.LoadSettings(); err == nil {
			a.compactTokens = resolveCompactTokens(s, a.Model.ContextWindow)
		}
	} else {
		a.compactTokens = -1
	}
	return nil
}

// SetLogRetention persists the retention period (0 deletes the key —
// never prune) and prunes immediately.
func (a *App) SetLogRetention(days int) error {
	if days < 0 || days > 3650 {
		return fmt.Errorf("retention days %d out of range (0–3650)", days)
	}
	var v any
	if days > 0 {
		v = days
	}
	if err := config.SetTUIConfigKey("logRetentionDays", v); err != nil {
		return err
	}
	if a.Store != nil && a.Sess != nil {
		a.Store.PruneOlderThan(days, a.Sess.Path)
	}
	return nil
}

// SetConfigBool persists one of the panel booleans (memory/checkpoint
// switches: the durable config their subsystems read).
func (a *App) SetConfigBool(key string, on bool) error {
	return config.SetTUIConfigKey(key, on)
}

// SetLanguage persists the UI language override ("" deletes the key =
// system auto-detect). The TUI applies it live via i18n.Set.
func (a *App) SetLanguage(lang string) error {
	var v any
	if lang != "" {
		v = lang
	}
	return config.SetTUIConfigKey("language", v)
}

// ConfigListText renders the /config listing (REPL surface).
func (a *App) ConfigListText() string {
	c := a.TUIConfig()
	onOff := func(b bool) string {
		if b {
			return i18n.T("tui.config.on")
		}
		return i18n.T("tui.config.off")
	}
	ret := i18n.T("tui.config.never")
	if c.LogRetentionDays > 0 {
		ret = i18n.Tf("tui.config.days", c.LogRetentionDays)
	}
	return i18n.Tf("cli.run.configList",
		onOff(c.AutoCompact), ret, onOff(c.AutoMemory), onOff(c.TypedMemory),
		onOff(c.MemoryRelevance), onOff(c.MemoryAutoExtract), onOff(c.RewindCheckpoints),
		onOff(c.ClipboardWatch), LanguageLabel(c.Language))
}

// LanguageLabel renders the language setting for the /config surfaces:
// an explicit override by name, "system (<detected>)" when unset.
func LanguageLabel(lang string) string {
	langName := func(l i18n.Lang) string {
		if l == i18n.Zh {
			return i18n.T("tui.config.langZh")
		}
		return i18n.T("tui.config.langEn")
	}
	if l, ok := i18n.Parse(lang); ok {
		return langName(l)
	}
	return i18n.Tf("tui.config.langAuto", langName(i18n.Auto()))
}

// planDelta builds the transcript system message carrying the plan
// section change (a later same-name section wins; Delete removes it).
func planDelta(active bool) llm.Message {
	sec := llm.Section{Name: planmode.Section, Value: planmode.Guidance}
	if !active {
		sec = llm.Section{Name: planmode.Section, Delete: true}
	}
	return llm.Message{Role: llm.RoleSystem, Sections: []llm.Section{sec}}
}

// SandboxPolicy resolves the effective per-call policy: session override
// over deployment default, workspace root = the session's immutable cwd
// (dsh's resolve()).
func (a *App) SandboxPolicy() sandbox.Policy {
	m := a.sandboxOverride
	if m == "" {
		m = a.sandboxDefault
	}
	if m == "" {
		m = sandbox.ModeWorkspaceWrite
	}
	return sandbox.Policy{Mode: m, WorkspaceRoot: a.CWD}
}

// SandboxMode reports the effective mode for clients (session/sandbox RPC).
func (a *App) SandboxMode() string { return string(a.SandboxPolicy().Mode) }

// SetSandboxMode switches the session's sandbox mode: the transition
// persists an append-only sandbox entry (replay restores it) and lands
// as a transcript section delta — same discipline as plan mode.
func (a *App) SetSandboxMode(mode string) error {
	m, err := sandbox.ParseMode(mode)
	if err != nil {
		return err
	}
	a.sandboxOverride = m
	if err := a.persist(session.Entry{Sandbox: &session.SandboxEntry{Mode: string(m)}}); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox mode persist: %v\n", err)
	}
	if a.Tr != nil {
		if err := a.Tr.Append(sandboxDelta(a.SandboxPolicy())); err != nil {
			fmt.Fprintf(os.Stderr, "sandbox mode delta: %v\n", err)
		}
	}
	return nil
}

// sandboxDelta builds the transcript system message carrying the sandbox
// policy section (confined modes only; danger-full-access deletes it).
func sandboxDelta(p sandbox.Policy) llm.Message {
	sec := llm.Section{Name: "sandbox", Value: sandbox.PromptSection(p)}
	if sec.Value == "" {
		sec = llm.Section{Name: "sandbox", Delete: true}
	}
	return llm.Message{Role: llm.RoleSystem, Sections: []llm.Section{sec}}
}

// protocolOf maps a provider key onto its wire protocol. Custom
// provider keys carry an explicit Protocol (openai-compat default);
// built-in aliases normalize to their canonical protocol.
func protocolOf(settings *config.Settings, providerName string) string {
	if pc, ok := settings.Providers[providerName]; ok && pc.Protocol != "" {
		switch pc.Protocol {
		case "openai":
			return "openai-compat"
		case "azure":
			return "azure-openai-responses"
		case "gemini":
			return "google"
		}
		return pc.Protocol
	}
	switch providerName {
	case "openai":
		return "openai-compat"
	case "azure":
		return "azure-openai-responses"
	case "gemini":
		return "google"
	}
	return providerName
}

func apiShape(providerName string) string {
	switch providerName {
	case "anthropic":
		return "anthropic-messages"
	case "google":
		return "google-generative-ai" // pi's api id differs from the provider id
	case "openai-responses", "azure-openai-responses":
		return providerName // provider name IS pi's api id
	default:
		return "openai-completions"
	}
}

// compactPrefix builds the conversation context at the cut point for
// prefix-reusing summary calls (auto-cache providers): the folded
// system message and current tool set — byte-identical to the main
// requests' — plus the projected messages before the cut.
func compactPrefix(tr *llm.Transcript, entries []session.Entry) []llm.Message {
	msgs := tr.Messages()
	sys := llm.CurrentSystemMessage(msgs)
	if sys == nil {
		return nil
	}
	lead := *sys
	lead.ToolsAdded = llm.CurrentTools(msgs)
	return append([]llm.Message{lead}, session.Project(entries)...)
}

// resolveCompactTokens picks the compaction threshold: an explicit
// setting wins (negative disables); else the model's context window
// minus reserve; else the flat default.
func resolveCompactTokens(s *config.Settings, window int) int {
	if s.CompactionTokens != 0 {
		return s.CompactionTokens
	}
	if window > 0 {
		t := window - session.DefaultReserveTokens
		if t < 16_000 {
			t = 16_000 // floor for tiny windows
		}
		return t
	}
	return session.DefaultCompactionTokens
}

// resolveKeepRecent picks the verbatim tail size kept across
// compaction: an explicit setting wins (negative keeps nothing); else
// pi's default.
func resolveKeepRecent(s *config.Settings) int {
	if s.KeepRecentTokens != 0 {
		return s.KeepRecentTokens
	}
	return session.DefaultKeepRecentTokens
}

// resolveThinking: flag > settings default > provider default (empty).
func resolveThinking(s *config.Settings, flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return s.DefaultThinkingLevel
}

// persist appends one entry to the session file and the in-memory
// mirror (the file stamps id/parent/timestamp on write).
func (a *App) persist(e session.Entry) error {
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	if a.closed {
		// The session file is closed (delete/shutdown raced an in-flight
		// run). Drop the write like persistTitle does instead of failing
		// the dying turn with "file already closed".
		return nil
	}
	stamped, err := a.Sess.AppendEntry(e)
	if err != nil {
		return err
	}
	a.entries = append(a.entries, stamped)
	return nil
}

// maybeGenerateTitle kicks off a best-effort, asynchronous one-off LLM call
// that summarizes the first user message into a session title, persisted as
// a title entry. It runs alongside the turn so it never delays the reply;
// a failure leaves the session untitled (clients fall back to the first
// message). userText is the raw prompt, used only when the session is
// brand new.
func (a *App) maybeGenerateTitle(userText string) {
	if !a.autoTitle || a.titleGen.Load() != 0 {
		return
	}
	first := a.firstUserText()
	if strings.TrimSpace(first) == "" {
		first = userText
	}
	if strings.TrimSpace(first) == "" {
		return
	}
	if a.currentTitle() != "" {
		a.titleGen.Store(2)
		return
	}
	if !a.titleGen.CompareAndSwap(0, 1) {
		return
	}
	sess := a.Sess // the title belongs to THIS session, even if /new supersedes it mid-flight
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		title, err := a.Agent.GenerateTitle(ctx, first)
		if err != nil || title == "" {
			if err != nil {
				fmt.Fprintf(os.Stderr, "title generation: %v\n", err)
			}
			a.titleGen.Store(0) // allow a retry on a later turn
			return
		}
		if err := a.persistTitle(sess, title); err != nil {
			fmt.Fprintf(os.Stderr, "title persist: %v\n", err)
			a.titleGen.Store(0)
			return
		}
		a.titleGen.Store(2)
	}()
}

// persistTitle appends a title entry to the session file. It serializes
// with message writes via persistMu but deliberately does NOT touch the
// in-memory entry mirror, which is owned by the turn's goroutines. A
// title generated for a session the app has since left (/new) is
// dropped silently — writing it would mislabel the fresh session.
func (a *App) persistTitle(sess *session.Session, title string) error {
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	if a.closed || a.Sess != sess {
		return nil // session shut down or superseded: drop the title silently
	}
	_, err := a.Sess.AppendEntry(session.Entry{Title: &session.TitleEntry{Title: title}})
	return err
}

// currentTitle folds the title from the in-memory entry mirror.
func (a *App) currentTitle() string {
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	return session.CurrentTitle(a.entries)
}

// firstUserText returns the text of the earliest user message, or "".
func (a *App) firstUserText() string {
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	for _, e := range a.entries {
		if e.Msg == nil || e.Msg.Role != llm.RoleUser {
			continue
		}
		var sb strings.Builder
		for _, b := range e.Msg.Content {
			if b.Kind == llm.BlockText {
				sb.WriteString(b.Text)
			}
		}
		if s := strings.TrimSpace(sb.String()); s != "" {
			return s
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// MCP hooks (generation swaps land mid-session as transcript deltas; scode's
// storage boundary keeps system messages request-time only, so deltas never
// touch the session file — a resume re-discovers everything at setup)
// ---------------------------------------------------------------------------

// onMCPToolsChanged applies one server's generation swap (dsh's phase-2):
// the registry reflects the new generation, and the change lands on the
// transcript as a system delta so the next request's tool list follows
// (CurrentTools replays deltas; the prefix stays byte-stable).
func (a *App) onMCPToolsChanged(server string, added []agent.Tool, removed []string) {
	a.mcpMu.Lock()
	defer a.mcpMu.Unlock()
	for _, name := range removed {
		a.mcpRegistry.Remove(name)
	}
	for _, t := range added {
		a.mcpRegistry.Add(t)
	}
	if a.Tr == nil || (len(added) == 0 && len(removed) == 0) {
		return // pre-transcript (initial sync during Setup): registry is enough
	}
	msg := llm.Message{Role: llm.RoleSystem}
	for _, t := range added {
		msg.ToolsAdded = append(msg.ToolsAdded, t.Decl())
	}
	msg.ToolsRemoved = removed
	if err := a.Tr.Append(msg); err != nil {
		fmt.Fprintf(os.Stderr, "mcp(%s): tool delta rejected: %v\n", server, err)
	}
}

// onMCPInstructionsChanged patches the prompt's mcp section via a
// transcript section delta (CurrentSystemMessage replays sections by
// name, later wins — empty text deletes the section).
func (a *App) onMCPInstructionsChanged(server, text string) {
	a.mcpMu.Lock()
	defer a.mcpMu.Unlock()
	if a.mcpInstructs == nil {
		a.mcpInstructs = map[string]string{}
	}
	if text == "" {
		delete(a.mcpInstructs, server)
	} else {
		a.mcpInstructs[server] = text
	}
	if a.Tr == nil {
		return
	}
	value := mcpInstructionsText(a.mcpInstructs)
	msg := llm.Message{
		Role: llm.RoleSystem,
		Sections: []llm.Section{
			{Name: "mcp", Value: value, Delete: value == ""},
		},
	}
	if err := a.Tr.Append(msg); err != nil {
		fmt.Fprintf(os.Stderr, "mcp(%s): instructions delta rejected: %v\n", server, err)
	}
}

// ---------------------------------------------------------------------------
// Runtime MCP configuration (/mcp). The file is the source of truth;
// the manager mirrors it. Every mutation is effective immediately:
// ToolsChanged/InstructionsChanged append transcript deltas exactly as
// a reconnect would, so new tools reach the model on the NEXT request —
// safe even mid-run (the hooks are mutex-serialized and the agent loop
// re-reads the tool set per request).
// ---------------------------------------------------------------------------

// mcpTransportHook, when set, replaces real MCP transports with
// in-memory ones (test seam; nil in production).
var mcpTransportHook mcp.TransportHook

// mcpDefaultPolicy maps a server's defaultPolicy string to its verdict
// ("" and "ask" → Ask) — setup and runtime adds share the rules.
func mcpDefaultPolicy(name, raw string) (permission.Decision, error) {
	switch raw {
	case "allow":
		return permission.Allow, nil
	case "deny":
		return permission.Deny, nil
	case "", "ask":
		return permission.Ask, nil
	default:
		return permission.Ask, fmt.Errorf("mcp.json: server %q: unknown defaultPolicy %q", name, raw)
	}
}

func (a *App) mcpPath() string { return filepath.Join(a.CfgDir, "mcp.json") }

// newMCPManager builds a manager with the stored hooks and the test
// transport seam.
func (a *App) newMCPManager(f *mcp.File) (*mcp.Manager, error) {
	mgr, err := mcp.NewManager(f, a.mcpHooks)
	if err != nil {
		return nil, err
	}
	if mcpTransportHook != nil {
		mgr.SetTransportHook(mcpTransportHook)
	}
	return mgr, nil
}

// ensureMCPManager lazily creates the manager — setup skips it when
// mcp.json has no servers, so the first runtime add builds it.
func (a *App) ensureMCPManager() error {
	if a.mcpManager != nil {
		return nil
	}
	mgr, err := a.newMCPManager(&mcp.File{})
	if err != nil {
		return err
	}
	if a.mcpInstructs == nil {
		a.mcpInstructs = map[string]string{}
	}
	a.mcpManager = mgr
	mgr.Start()
	return nil
}

// validateMCPServer checks ONE entry against the file-level rules.
func validateMCPServer(name string, cfg mcp.ServerConfig) error {
	return (&mcp.File{MCPServers: map[string]mcp.ServerConfig{name: cfg}}).Validate()
}

// setMCPPermission refreshes the server's fallback-tier rule (remove
// first: re-adding must not pile up duplicates).
func (a *App) setMCPPermission(name string, cfg mcp.ServerConfig) error {
	policy, err := mcpDefaultPolicy(name, cfg.DefaultPolicy)
	if err != nil {
		return err
	}
	pattern := "mcp__" + name + "__*"
	a.perms.RemoveDefault(pattern)
	return a.perms.AddDefault(pattern, policy)
}

// MCPServerInfo is one configured server plus its live status.
type MCPServerInfo struct {
	Name   string
	Config mcp.ServerConfig
	Status *mcp.Status // nil when the server is not running (disabled)
}

// MCPServers lists every configured server, sorted by name, with live
// status for the running ones. A file read error yields an empty list
// (the TUI treats it as "no servers").
func (a *App) MCPServers() []MCPServerInfo {
	f, err := mcp.LoadFile(a.mcpPath())
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(f.MCPServers))
	for name := range f.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]MCPServerInfo, 0, len(names))
	for _, name := range names {
		info := MCPServerInfo{Name: name, Config: f.MCPServers[name]}
		if a.mcpManager != nil {
			if st, ok := a.mcpManager.Status(name); ok {
				info.Status = &st
			}
		}
		out = append(out, info)
	}
	return out
}

// MCPAdd persists and starts one NEW server (creating the manager on
// first use). Existing names are rejected — edits go through
// MCPRestart. All validation runs BEFORE the file is touched.
func (a *App) MCPAdd(name string, cfg mcp.ServerConfig) error {
	if err := validateMCPServer(name, cfg); err != nil {
		return err
	}
	if _, err := mcpDefaultPolicy(name, cfg.DefaultPolicy); err != nil {
		return err
	}
	if f, err := mcp.LoadFile(a.mcpPath()); err != nil {
		return err
	} else if _, exists := f.MCPServers[name]; exists {
		return fmt.Errorf("mcp: server %q already exists — edit it instead", name)
	}
	if err := mcp.UpsertServer(a.mcpPath(), name, cfg); err != nil {
		return err
	}
	if err := a.ensureMCPManager(); err != nil {
		return err
	}
	if !cfg.IsEnabled() { // persisted disabled: nothing to start
		return a.setMCPPermission(name, cfg)
	}
	if err := a.mcpManager.Add(name, cfg); err != nil {
		return err
	}
	return a.setMCPPermission(name, cfg)
}

// MCPRestart edits one server: persist, dispose the old supervisor,
// start a fresh one (shutdown precedes start so the old generation's
// removal delta can never unregister the new generation's tools).
// Disabling through an edit only persists — no supervisor runs.
func (a *App) MCPRestart(name string, cfg mcp.ServerConfig) error {
	if err := validateMCPServer(name, cfg); err != nil {
		return err
	}
	if _, err := mcpDefaultPolicy(name, cfg.DefaultPolicy); err != nil {
		return err
	}
	if err := mcp.UpsertServer(a.mcpPath(), name, cfg); err != nil {
		return err
	}
	if err := a.ensureMCPManager(); err != nil {
		return err
	}
	if cfg.IsEnabled() {
		if err := a.mcpManager.Restart(name, cfg); err != nil {
			return err
		}
	} else {
		a.mcpManager.Remove(name)
	}
	return a.setMCPPermission(name, cfg)
}

// MCPRemove deletes one server: dispose the supervisor (tools
// unregister), forget the config entry and its permission default.
func (a *App) MCPRemove(name string) error {
	if err := mcp.DeleteServer(a.mcpPath(), name); err != nil {
		return err
	}
	if a.mcpManager != nil {
		a.mcpManager.Remove(name)
	}
	a.perms.RemoveDefault("mcp__" + name + "__*")
	return nil
}

// MCPToggle flips a server's enabled flag losslessly: off disposes the
// supervisor (the config stays in mcp.json), on starts it.
func (a *App) MCPToggle(name string) (enabled bool, err error) {
	f, err := mcp.LoadFile(a.mcpPath())
	if err != nil {
		return false, err
	}
	cfg, ok := f.MCPServers[name]
	if !ok {
		return false, fmt.Errorf("mcp: server %q is not configured", name)
	}
	on := !cfg.IsEnabled()
	cfg.Enabled = &on
	if err := mcp.UpsertServer(a.mcpPath(), name, cfg); err != nil {
		return false, err
	}
	if err := a.ensureMCPManager(); err != nil {
		return false, err
	}
	if on {
		if err := a.mcpManager.Restart(name, cfg); err != nil {
			return false, err
		}
	} else {
		a.mcpManager.Remove(name)
	}
	return on, nil
}

// mcpListText renders the configured servers for the REPL (the TUI's
// /mcp overlay manages them interactively).
func (a *App) mcpListText() string {
	servers := a.MCPServers()
	if len(servers) == 0 {
		return "mcp: no servers configured — the TUI's /mcp command configures them interactively (file: " + a.mcpPath() + ")"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "mcp servers (%s):\n", a.mcpPath())
	for _, s := range servers {
		state := i18n.T("tui.mcp.statusDisabled")
		if s.Status != nil {
			state = i18n.Tf("cli.run.mcpState", s.Status.State, s.Status.Tools)
		}
		fmt.Fprintf(&b, "  %-20s %-14s %s\n", s.Name, s.Config.Transport, state)
	}
	return strings.TrimRight(b.String(), "\n")
}

// mcpInstructionsText renders the mcp prompt section from per-server
// blocks (sorted for determinism).
func mcpInstructionsText(blocks map[string]string) string {
	if len(blocks) == 0 {
		return ""
	}
	names := make([]string, 0, len(blocks))
	for name := range blocks {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, blocks[name])
	}
	return strings.Join(parts, "\n\n")
}

// ---------------------------------------------------------------------------
// Skill hot reload (/reload re-runs discovery and patches the prompt's
// skills section via a transcript delta — same discipline as MCP
// instructions; a failed /skill:name lookup reloads lazily, so normal
// prompts never scan the skill tree)
// ---------------------------------------------------------------------------

// skillList snapshots the current skills under the read lock (steering
// reads them while a run — and possibly a reload — is in flight).
func (a *App) skillList() []skills.Skill {
	a.skillsMu.RLock()
	defer a.skillsMu.RUnlock()
	return a.skills
}

// maybeReloadSkills was the eager half of hot reload (a fingerprint
// check at every prompt boundary); the lazy design below replaced it:
// normal prompts pay nothing, and only a FAILED /skill:name lookup
// triggers one fingerprint check + reload + retry.

func (a *App) skillFingerprint() uint64 {
	a.skillsMu.RLock()
	defer a.skillsMu.RUnlock()
	return a.skillFp
}

// expandSkill expands /skill:name against the live set (pi's
// _expandSkillCommand). An unknown name triggers one LAZY reload: the
// tree is fingerprinted (cheap walk, no file reads), and only a
// mismatch re-runs discovery and retries the expansion once — a skill
// added mid-session works with no restart and no per-prompt rescan.
// Still-unknown commands pass through unchanged, like pi.
func (a *App) expandSkill(text string) string {
	out := skills.ExpandCommand(text, a.skillList())
	if out != text || !strings.HasPrefix(text, "/skill:") {
		return out // expanded, or not a skill command at all
	}
	if skills.Fingerprint(a.skillOpts) == a.skillFingerprint() {
		return text // genuinely unknown: the tree matches the loaded set
	}
	fmt.Fprintln(os.Stderr, "skills: "+a.ReloadSkills())
	return skills.ExpandCommand(text, a.skillList())
}

// ReloadSkills re-runs skill discovery with the setup-time options and
// swaps the live set: the system prompt's skills section is patched via
// a transcript section delta (CurrentSystemMessage replays by name;
// empty deletes), so the next request sees the new listing and
// /skill:name expands against it. Returns a human summary.
func (a *App) ReloadSkills() string {
	res := skills.Load(a.skillOpts)
	for _, d := range res.Diagnostics {
		fmt.Fprintf(os.Stderr, "skills: %s: %s (%s)\n", d.Type, d.Message, d.Path)
	}

	a.skillsMu.Lock()
	old := a.skills
	a.skills = res.Skills
	a.skillFp = skills.Fingerprint(a.skillOpts)
	a.skillsMu.Unlock()

	value := skills.FormatForPrompt(res.Skills)
	if a.Tr != nil {
		msg := llm.Message{
			Role: llm.RoleSystem,
			Sections: []llm.Section{
				{Name: "skills", Value: value, Delete: value == ""},
			},
		}
		if err := a.Tr.Append(msg); err != nil {
			fmt.Fprintf(os.Stderr, "skills: section delta rejected: %v\n", err)
		}
	}

	oldNames := map[string]bool{}
	for _, s := range old {
		oldNames[s.Name] = true
	}
	newNames := map[string]bool{}
	var added []string
	for _, s := range res.Skills {
		newNames[s.Name] = true
		if !oldNames[s.Name] {
			added = append(added, s.Name)
		}
	}
	var removed []string
	for _, s := range old {
		if !newNames[s.Name] {
			removed = append(removed, s.Name)
		}
	}
	summary := fmt.Sprintf("reloaded: %d skill(s)", len(res.Skills))
	if len(added) > 0 {
		summary += " (added: " + strings.Join(added, ", ") + ")"
	}
	if len(removed) > 0 {
		summary += " (removed: " + strings.Join(removed, ", ") + ")"
	}
	if len(added) == 0 && len(removed) == 0 {
		summary += " (no changes)"
	}
	return summary
}

// Run executes one prompt under ctx (cancellation aborts the run).
// Agent events stream through to out (closed on return). images are
// explicit attachments (the TUI's clipboard images); image file paths
// referenced in the prompt text are scanned and attached too. A
// provider context-overflow failure recovers like pi: compact once,
// then retry the failed turn from the rebuilt projection.
func (a *App) Run(ctx context.Context, out chan<- agent.Event, promptText string, images ...llm.Block) error {
	defer close(out)
	// The raw prompt names the session before command expansion bloats it.
	rawPrompt := promptText
	// Command expansion happens at the prompt boundary (pi's
	// _expandSkillCommand in agent-session.prompt): built-in prompt
	// commands (/commit, /commit-push-pr) become canned workflows, then
	// /skill:name expands; unknown commands pass through unchanged.
	if expanded, ok := expandPromptCommand(promptText); ok {
		promptText = expanded
	} else {
		promptText = a.expandSkill(promptText)
	}
	// Attachments: paths scanned from the RAW text (the expanded skill
	// body may mention files the user did not intend to attach), then
	// explicit clipboard images.
	images = append(ScanImagePaths(rawPrompt, a.CWD), images...)
	if strings.TrimSpace(promptText) == "" && len(images) > 0 {
		promptText = "(see attached image)"
	}
	content := make([]llm.Block, 0, len(images)+1)
	content = append(content, llm.TextBlock(promptText))
	content = append(content, images...)
	a.maybeGenerateTitle(rawPrompt)
	err := a.drive(ctx, out, content)
	if isOverflowError(err) {
		fmt.Fprintln(os.Stderr, "context overflow — compacting and retrying")
		if cerr := a.compactNow(ctx, ""); cerr != nil {
			fmt.Fprintf(os.Stderr, "overflow recovery failed: %v\n", cerr)
			return err
		}
		err = a.drive(ctx, out, nil) // continue from the recorded failure turn
	}
	return err
}

// overflowPattern matches provider failures caused by an over-long
// context (pi's isContextOverflow): recoverable by compaction, unlike
// the other non-retryable classes.
var overflowPattern = regexp.MustCompile(`(?i)(context[ _]length|context[ _]window|prompt[ _]is[ _]too[ _]long|input[ _]is[ _]too[ _]long|too[ _]many[ _]tokens|exceeds?.*(token|context)|maximum[ _]context)`)

func isOverflowError(err error) bool {
	return err != nil && overflowPattern.MatchString(err.Error())
}

// drive runs one attempt: with content it starts a new prompt;
// empty continues from the transcript tail (the overflow retry).
// New transcript messages persist incrementally at message boundaries —
// every finished turn is durable before the next one starts (pi
// persists on message_end), so a crash mid-run loses nothing already
// completed. Afterwards compaction fires if the projected context
// exceeds the threshold.
func (a *App) drive(ctx context.Context, out chan<- agent.Event, content []llm.Block) error {
	inner := make(chan agent.Event, 256)
	done := make(chan error, 1)
	if len(content) == 0 {
		go func() { done <- a.Agent.Continue(ctx, a.Tr, inner) }()
	} else {
		go func() { done <- a.Agent.PromptBlocks(ctx, a.Tr, content, inner) }()
	}

	var persistErr error
	sync := func() {
		if err := a.syncTranscript(); err != nil && persistErr == nil {
			persistErr = err
		}
	}
	for ev := range inner {
		switch ev.Type {
		case agent.EvUser, agent.EvAssistant, agent.EvTurnEnd, agent.EvAgentError:
			sync() // the transcript just grew: flush the delta
		}
		out <- ev
	}
	err := <-done
	sync() // final messages (error turn, truncation recovery)
	if err != nil {
		return err
	}
	if persistErr != nil {
		return persistErr
	}
	a.maybeCompact(ctx)
	return nil
}

// syncTranscript persists every transcript message not yet on disk and
// folds its usage into the session totals. The watermark advances only
// on success, so a failed write retries on the next sync instead of the
// message being silently skipped.
func (a *App) syncTranscript() error {
	msgs := a.Tr.Messages()
	for a.persisted < len(msgs) {
		m := msgs[a.persisted]
		if m.Role == llm.RoleSystem {
			// Request-time only: the prompt is rebuilt at load (pi's
			// storage boundary), never persisted.
			a.persisted++
			continue
		}
		if m.Usage != nil {
			m.Usage.CostUSD = m.Usage.Cost(a.pricing) // idempotent stamp of the persisted copy
		}
		if err := a.persist(session.MsgEntry(m)); err != nil {
			return err
		}
		if m.Usage != nil {
			a.spent = a.spent.Add(*m.Usage)
		}
		a.persisted++
	}
	return nil
}

// UsageReport is the structured session usage snapshot for clients:
// context occupancy against the model window plus accumulated
// token/cache/cost totals (drives the desktop's context-% and
// cache-hit-rate displays).
type UsageReport struct {
	ContextTokens int64   `json:"contextTokens"`
	ContextWindow int     `json:"contextWindow"`
	Input         int64   `json:"input"`
	Output        int64   `json:"output"`
	CacheRead     int64   `json:"cacheRead"`
	CacheWrite    int64   `json:"cacheWrite"`
	CacheWrite1h  int64   `json:"cacheWrite1h"`
	CostUSD       float64 `json:"costUSD"`
	// Breakdown splits ContextTokens into coarse categories for the
	// desktop's hover card; nil on estimate failure.
	Breakdown *ContextBreakdown `json:"breakdown,omitempty"`
}

// ContextBreakdown is the per-category context occupancy (chars/4
// heuristics, same basis as EstimateTokens): 消息/系统工具/系统提示词/
// 技能/MCP 工具/其他. "Other" absorbs the estimate-vs-billed gap so the
// rows sum to ContextTokens.
type ContextBreakdown struct {
	Messages  int64 `json:"messages"`  // conversation (user/assistant/tool)
	SysTools  int64 `json:"sysTools"`  // built-in tool declarations + tools/rules sections
	SysPrompt int64 `json:"sysPrompt"` // preamble + project_context/cwd sections
	Skills    int64 `json:"skills"`    // skills section
	McpTools  int64 `json:"mcpTools"`  // mcp section + mcp__ tool declarations
	Other     int64 `json:"other"`     // runtime-state sections + estimation gap
}

// UsageReport snapshots the current context occupancy (last valid usage
// + chars/4 tail estimate, same basis as the compaction trigger) and
// the session's accumulated spend.
func (a *App) UsageReport() UsageReport {
	msgs := a.Tr.Messages()
	total := session.EstimateContextTokens(msgs)
	return UsageReport{
		ContextTokens: total,
		ContextWindow: a.Model.ContextWindow,
		Input:         a.spent.Input,
		Output:        a.spent.Output,
		CacheRead:     a.spent.CacheRead,
		CacheWrite:    a.spent.CacheWrite,
		CacheWrite1h:  a.spent.CacheWrite1h,
		CostUSD:       a.spent.CostUSD,
		Breakdown:     contextBreakdown(msgs, total),
	}
}

// contextBreakdown estimates the per-category occupancy behind the
// context total. Sections and tool declarations come from the replayed
// system message (CurrentSystemMessage — what the wire actually sends);
// the conversation is estimated message-by-message. Runtime-state
// sections (plan/sandbox) have no card row of their own and land in
// "other" together with the gap between the chars/4 estimates and the
// billed total.
func contextBreakdown(msgs []llm.Message, total int64) *ContextBreakdown {
	b := &ContextBreakdown{}
	if sys := llm.CurrentSystemMessage(msgs); sys != nil {
		b.SysPrompt = int64(session.EstimateTokens(*sys))
		for _, sec := range sys.Sections {
			// wire shape: "<name>\n" + value + "\n</name>" (renderSystem)
			chars := len(sec.Value) + 2*len(sec.Name) + 7
			n := int64((chars + 3) / 4)
			switch sec.Name {
			case "tools", "rules":
				b.SysTools += n
			case "skills":
				b.Skills += n
			case "mcp":
				b.McpTools += n
			case "project_context", "cwd":
				b.SysPrompt += n
			default:
				b.Other += n
			}
		}
		for _, t := range sys.ToolsAdded {
			decl, err := llm.DeclarationBytes(t)
			if err != nil {
				continue
			}
			n := int64((len(decl) + 3) / 4)
			if strings.HasPrefix(t.Name, "mcp__") {
				b.McpTools += n
			} else {
				b.SysTools += n
			}
		}
	}
	for _, m := range msgs {
		if m.Role != llm.RoleSystem {
			b.Messages += int64(session.EstimateTokens(m))
		}
	}
	if gap := total - (b.Messages + b.SysTools + b.SysPrompt + b.Skills + b.McpTools); gap > 0 {
		b.Other += gap
	}
	return b
}

// CostReport renders every category the usage kernel tracks: the
// session's token accounting per cache class and cost, the current
// context occupancy against the window with the cache hit rate, and
// the per-category context breakdown (the desktop hover card's rows).
func (a *App) CostReport() string {
	r := a.UsageReport()
	var b strings.Builder
	fmt.Fprintf(&b, "%s", i18n.Tf("cli.run.usage",
		commaInt(r.Input), commaInt(r.Output), commaInt(r.CacheRead), commaInt(r.CacheWrite)))
	if r.CacheWrite1h > 0 {
		fmt.Fprintf(&b, "%s", i18n.Tf("cli.run.usageCache1h", commaInt(r.CacheWrite1h)))
	}
	if (a.pricing != llm.Pricing{}) {
		fmt.Fprintf(&b, "%s", i18n.Tf("cli.run.usageCost", r.CostUSD))
	}
	if r.ContextWindow > 0 {
		pct := 100 * float64(r.ContextTokens) / float64(r.ContextWindow)
		fmt.Fprintf(&b, "%s", i18n.Tf("cli.run.usageCtx", commaInt(r.ContextTokens), commaInt(int64(r.ContextWindow)), pct))
	} else {
		fmt.Fprintf(&b, "%s", i18n.Tf("cli.run.usageCtxUnknown", commaInt(r.ContextTokens)))
	}
	if reads := r.Input + r.CacheRead; reads > 0 {
		fmt.Fprintf(&b, "%s", i18n.Tf("cli.run.usageCacheHit", (100*r.CacheRead+reads/2)/reads))
	}
	if bd := r.Breakdown; bd != nil {
		fmt.Fprintf(&b, "%s", i18n.Tf("cli.run.usageDetail",
			commaInt(bd.Messages), commaInt(bd.SysPrompt), commaInt(bd.SysTools), commaInt(bd.Skills), commaInt(bd.McpTools), commaInt(bd.Other)))
	}
	return b.String()
}

// commaInt renders n with thousands separators (token counts read in
// the millions; the naked digits do not).
func commaInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	if neg {
		return "-" + strings.Join(parts, ",")
	}
	return strings.Join(parts, ",")
}

// ErrNothingToCompact reports a history that already fits inside the
// kept tail — there is nothing older to summarize (pi's
// prepareCompaction returning undefined).
var ErrNothingToCompact = errors.New("nothing to compact")

// maybeCompact compacts when the projected context exceeds the token
// threshold. Failures skip this round (compaction retries naturally on
// the next prompt).
func (a *App) maybeCompact(ctx context.Context) {
	if !session.NeedsCompaction(a.entries, a.compactTokens) {
		return
	}
	if err := a.compactNow(ctx, ""); err != nil {
		fmt.Fprintf(os.Stderr, "compaction skipped: %v\n", err)
	}
}

// compactNow summarizes everything before the cut point, appends the
// marker (with the kept-tail index and tokensBefore), and rebuilds the
// projection. The recent ~keepRecentTokens tail stays verbatim (pi's
// partial compaction). custom carries optional user focus instructions
// (manual /compact).
func (a *App) compactNow(ctx context.Context, custom string) error {
	// pi's prepareCompaction guard: never compact directly on top of a
	// previous marker — there is no new conversation to fold in.
	if n := len(a.entries); n > 0 && a.entries[n-1].Compaction != nil {
		return ErrNothingToCompact
	}
	cut := session.FindCutPoint(a.entries, resolveKeepRecent(a.Settings))
	if cut <= 0 {
		return ErrNothingToCompact
	}
	// Summarize the conversation since the previous marker up to the
	// cut. pi's split-turn handling: when the cut lands mid-turn (not
	// on the user message that started it), the main summary stops at
	// the turn start and the turn prefix gets its own checkpoint.
	since := 0
	prevSummary := ""
	for i := len(a.entries) - 1; i >= 0; i-- {
		if c := a.entries[i].Compaction; c != nil {
			since, prevSummary = i+1, c.Summary
			break
		}
	}
	collect := func(lo, hi int) []llm.Message {
		var out []llm.Message
		for _, e := range a.entries[lo:hi] {
			if e.Msg != nil && e.Msg.Role != llm.RoleSystem {
				out = append(out, *e.Msg)
			}
		}
		return out
	}
	historyEnd := cut
	ts := cut // turn start, when the cut lands mid-turn
	var turnPrefix []llm.Message
	if cut < len(a.entries) {
		if m := a.entries[cut].Msg; m != nil && m.Role != llm.RoleUser {
			ts = since
			for j := cut; j >= since; j-- {
				if ej := a.entries[j].Msg; ej != nil && ej.Role == llm.RoleUser {
					ts = j
					break
				}
			}
			historyEnd = ts
			turnPrefix = collect(ts, cut)
		}
	}
	msgs := collect(since, historyEnd)
	if len(msgs) == 0 && len(turnPrefix) == 0 {
		return ErrNothingToCompact
	}
	tokensBefore := session.EstimateProjectedTokens(a.entries)

	summary := ""
	ops := a.fileOps
	if len(msgs) > 0 || prevSummary != "" {
		s, o, err := a.Agent.Compact(ctx, msgs, compactPrefix(a.Tr, a.entries[:cut]), prevSummary, custom, a.fileOps)
		if err != nil {
			return err
		}
		summary, ops = s, o
	}
	if len(turnPrefix) > 0 {
		prefix, err := a.Agent.CompactTurnPrefix(ctx, turnPrefix, compactPrefix(a.Tr, a.entries[:ts]))
		if err != nil {
			return err
		}
		if summary != "" {
			summary += "\n\n---\n\n**Turn Context (split turn):**\n\n" + prefix
		} else {
			summary = prefix
		}
		ops = session.ExtractFileOps(turnPrefix, ops)
	}
	a.fileOps = ops
	marker := session.NewCompaction(summary, ops.Read, ops.Modified)
	// keepRecentTokens <= 0 cuts past the last entry (keep nothing):
	// no kept-tail entry exists and the empty id projects exactly the
	// marker (total compaction) — indexing at cut would panic.
	if cut < len(a.entries) {
		marker.FirstKeptEntryID = a.entries[cut].ID // pi's firstKeptEntryId
	}
	marker.TokensBefore = int(tokensBefore)
	if err := a.persist(session.Entry{Compaction: &marker}); err != nil {
		return fmt.Errorf("compaction persist failed: %w", err)
	}
	msgs2 := session.Project(a.entries)
	tr, err := llm.NewTranscript(a.sysMsg, msgs2...)
	if err != nil {
		return err
	}
	a.Tr = tr
	a.persisted = tr.Len() // the projection is already on disk
	return nil
}

// CompactNow exposes manual compaction to non-CLI frontends (serve).
func (a *App) CompactNow(ctx context.Context, custom string) error {
	return a.compactNow(ctx, custom)
}

// CompactNowReport is CompactNow with a token summary for clients: the
// estimated projected tokens before and after the compaction (equal when
// nothing was compacted).
func (a *App) CompactNowReport(ctx context.Context, custom string) (before, after int64, err error) {
	before = session.EstimateProjectedTokens(a.entries)
	err = a.compactNow(ctx, custom)
	after = session.EstimateProjectedTokens(a.entries)
	return before, after, err
}

// SetPlanMode switches plan mode; reports whether the state changed.
func (a *App) SetPlanMode(on bool) bool { return a.planCtl.Set(on) }

// PlanMode reports the current mode.
func (a *App) PlanMode() bool { return a.planCtl.Active() }

// CurrentModel reports the active provider/model.
func (a *App) CurrentModel() (provider, model string) {
	return a.Model.Provider, a.Model.ID
}

// ConfiguredModels lists the switchable provider/model pairs from the
// freshest settings (profiles edited since startup become selectable
// without a restart) — the same basis as the server's models/list.
func (a *App) ConfiguredModels() []config.ModelChoice {
	settings := a.Settings
	if fresh, err := config.LoadSettings(); err == nil {
		settings = fresh
	}
	return config.ListModels(settings)
}

// PersistDefaultModel records the provider/model as the settings-level
// default — the fallback fresh sessions (and the next launch) resolve.
// Switch surfaces that "remember the choice" (the /model command) call
// this after a successful switch; the desktop's per-session switch
// deliberately does not.
func (a *App) PersistDefaultModel(provider, model string) error {
	return config.SetDefault(provider, model)
}

// PersistDefaultThinking records the reasoning effort as the
// settings-level default ("" removes the key: provider default).
func (a *App) PersistDefaultThinking(level string) error {
	return config.SetDefaultThinking(level)
}

// CurrentThinkingLevel reports the session's reasoning effort (""
// means the provider default).
func (a *App) CurrentThinkingLevel() string {
	return a.thinkingLevel
}

// SetThinkingLevel switches the reasoning effort for the live session
// and persists it as an append-only thinking entry, so resume replays
// it. Like SetModel, callers must ensure no run is in flight. Level ""
// resets to the provider default behavior.
func (a *App) SetThinkingLevel(level string) error {
	switch level {
	case "", "off", "low", "medium", "high":
	default:
		return fmt.Errorf("unknown thinking level %q (want off|low|medium|high)", level)
	}
	a.thinkingLevel = level
	if a.Agent != nil {
		a.Agent.SetThinkingLevel(level)
	}
	if err := a.persist(session.Entry{Thinking: &session.ThinkingEntry{Level: level}}); err != nil {
		fmt.Fprintf(os.Stderr, "thinking change persist: %v\n", err)
	}
	return nil
}

// SetModel switches the provider/model for the live session. Callers
// must ensure no run is in flight (the agent loop reads its config
// without a lock). The switch is persisted as an append-only model
// entry, so resume replays it. Empty provider/model fall back to the
// configured defaults, exactly like startup resolution.
func (a *App) SetModel(providerName, modelName string) (string, string, error) {
	// Resolve against fresh settings: profiles added/edited via the
	// settings UI after this session started must be selectable.
	settings := a.Settings
	if fresh, err := config.LoadSettings(); err == nil {
		settings = fresh
	}
	p, modelID, resolved, err := config.ResolveProvider(settings, providerName, modelName)
	if err != nil {
		return "", "", err
	}
	a.Settings = settings
	a.Provider = p
	a.Model = buildModel(settings, resolved, modelID)
	a.compactTokens = resolveCompactTokens(settings, a.Model.ContextWindow)
	a.pricing = llm.Pricing{}
	if pc := settings.Providers[resolved].Pricing; pc != nil {
		a.pricing = *pc
	}
	if a.Agent != nil {
		a.Agent.SetModel(p, a.Model, config.ShellEnv(a.Sess.Header().ID, resolved, modelID))
	}
	if err := a.persist(session.Entry{Model: &session.ModelEntry{Provider: resolved, Model: modelID}}); err != nil {
		fmt.Fprintf(os.Stderr, "model change persist: %v\n", err)
	}
	return a.Model.Provider, a.Model.ID, nil
}

// buildModel assembles the runtime model descriptor from settings. It
// mirrors the startup construction so live switches stay consistent.
func buildModel(settings *config.Settings, providerName, modelID string) llm.Model {
	pc := settings.Providers[providerName]
	protocol := protocolOf(settings, providerName)
	imageInput := protocol == "anthropic" // Claude is vision-capable; compat endpoints opt in
	if pc.ImageInput != nil {
		imageInput = *pc.ImageInput
	}
	return llm.Model{
		ID:            modelID,
		Provider:      providerName,
		APIShape:      apiShape(protocol),
		MaxTokens:     pc.MaxTokens,
		ContextWindow: pc.ContextWindow,
		Reasoning:     pc.Reasoning,
		Caps:          llm.Capabilities{ImageInput: imageInput, AutoCache: protocol != "anthropic", MidConvoSystem: pc.MidConvoSystem},
	}
}

// Close flushes state and shuts MCP servers down (dsh's plugin
// disposal: transports close, children terminate). This session's detached
// background commands are stopped too — losing a session must not orphan
// its processes — but only THIS session's: the registry is process-wide,
// and one App closing (delete, supersede) must not kill another session's
// tasks. Best-effort: the Windows sandbox Job is the hard net.
// MarkInteractive flags the surface as interactive (TUI entry point;
// the REPL sets the field directly) — gates close-time memory
// extraction so one-shot print runs never write memories.
func (a *App) MarkInteractive() { a.interactive = true }

// NewSession starts a fresh conversation in place (the /new command):
// the app rebinds to a new session file while the old one stays on
// disk, resumable. Conversation state resets (transcript, entry
// mirror, accounting, plan state, sandbox override, title); the
// process-level wiring carries over (provider/model, registry, MCP,
// skills, permissions, memory). The caller must guarantee no run is
// in flight (SetModel's contract).
func (a *App) NewSession() (oldID, newID string, err error) {
	sess, err := a.Store.Create("", a.CWD)
	if err != nil {
		return "", "", err
	}
	tr, err := llm.NewTranscript(a.sysMsg)
	if err != nil {
		sess.Close() //nolint:errcheck
		return "", "", err
	}
	// The session-scoped sandbox override dies with the old session;
	// the deployment default applies, landing as a section delta like
	// Setup's fresh-session path (request-time only).
	a.sandboxOverride = ""
	if pol := a.SandboxPolicy(); sandbox.PromptSection(pol) != "" {
		if err := tr.Append(sandboxDelta(pol)); err != nil {
			sess.Close() //nolint:errcheck
			return "", "", fmt.Errorf("sandbox section: %w", err)
		}
	}

	oldID = a.Sess.Header().ID
	// Detached background commands belong to the session that spawned
	// them — superseding it must not orphan its processes (Close's
	// rule, same registry scoping).
	tools.KillBackgroundTasksForSession(oldID)

	a.persistMu.Lock()
	old := a.Sess
	a.Sess = sess
	a.persistMu.Unlock()
	old.Close() //nolint:errcheck — the log stays resumable on disk

	a.Tr = tr
	a.entries = nil
	a.fileOps = session.FileOps{}
	a.persisted = 0 // the leading system message skips storage on the first sync
	a.spendMu.Lock()
	a.spent = llm.Usage{}
	a.spendMu.Unlock()
	a.planCtl.Restore(false) // the fresh transcript carries no plan delta
	a.planTrk.Reset()
	a.titleGen.Store(0)

	newID = sess.Header().ID
	// The cache-routing key and the tool env both name the session —
	// swap them for subsequent runs.
	a.Agent.SetSession(newID, config.ShellEnv(newID, a.Model.Provider, a.Model.ID))
	return oldID, newID, nil
}

func (a *App) Close() error {
	// Close-time memory extraction (memoryAutoExtraction on): a
	// best-effort side-channel call before anything tears down.
	a.maybeAutoExtractMemories()
	if a.mcpManager != nil {
		a.mcpManager.Shutdown()
	}
	if a.Sess != nil {
		tools.KillBackgroundTasksForSession(a.Sess.Header().ID)
	}
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	a.closed = true
	return a.Sess.Close()
}

// ListSessions prints the session ids stored for the current directory
// (no session is created).
func ListSessions(opts Options) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cfgDir, err := config.EnsureDir()
	if err != nil {
		return err
	}
	store, err := session.NewStore(session.DefaultRoot(cfgDir, cwd))
	if err != nil {
		return err
	}
	ids, err := store.List()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fmt.Println("(no sessions for this directory)")
		return nil
	}
	for _, id := range ids {
		fmt.Println(id)
	}
	return nil
}
