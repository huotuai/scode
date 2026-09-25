// Package cli wires config, prompt, agent, and session persistence into
// the user-facing entry points (print mode and REPL).
package cli

import (
	"context"
	"fmt"
	"os"

	"scode/internal/agent"
	"scode/internal/config"
	"scode/internal/llm"
	"scode/internal/prompt"
	"scode/internal/session"
	"scode/internal/tools"
)

// App is one running scode instance bound to a working directory.
//
// Storage discipline: a.entries mirrors the session file (full history,
// messages + compaction markers); a.Tr is the read-time PROJECTION the
// LLM actually sees. New messages produced by a run are persisted as a
// delta; compaction appends a marker and rebuilds the projection — the
// stored history is never rewritten.
type App struct {
	CWD      string
	CfgDir   string
	Provider llm.Provider
	Model    llm.Model
	Settings *config.Settings

	Agent         *agent.Agent
	Store         *session.Store
	Sess          *session.Session
	Tr            *llm.Transcript
	entries       []session.Entry
	fileOps       session.FileOps
	compactTokens int
}

// Options select provider/model/session at startup.
type Options struct {
	Provider      string
	Model         string
	Resume        string // session id
	ThinkingLevel string // off | low | medium | high
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
		CWD:      cwd,
		CfgDir:   cfgDir,
		Provider: p,
		Model:    llm.Model{ID: modelID, Provider: providerName, APIShape: apiShape(providerName)},
		Settings: settings,
	}
	if settings.CompactionTokens != 0 {
		a.compactTokens = settings.CompactionTokens
	} else {
		a.compactTokens = session.DefaultCompactionTokens
	}

	store, err := session.NewStore(session.DefaultRoot(cfgDir, cwd))
	if err != nil {
		return nil, err
	}
	a.Store = store

	sysMsg := prompt.Build(cfgDir, cwd, nil)

	// Tool declarations live in the leading system message — providers
	// read them from the transcript, so they must be attached here.
	registry := tools.NewCodingRegistry()
	sysMsg.ToolsAdded = registry.Decls()

	if opts.Resume != "" {
		rec, err := store.Load(opts.Resume)
		if err != nil {
			return nil, fmt.Errorf("resume %s: %w", opts.Resume, err)
		}
		a.entries = rec.Entries
		a.fileOps = rec.LatestFileOps()
		tr, err := rec.Transcript()
		if err != nil {
			return nil, err
		}
		a.Tr = tr
		// Continue appending to the ORIGINAL file: the id stays
		// resumable forever and a.entries mirrors what is on disk.
		sess, err := store.OpenForAppend(rec.Header.ID)
		if err != nil {
			return nil, err
		}
		a.Sess = sess
	} else {
		a.entries = []session.Entry{session.MsgEntry(sysMsg)}
		tr, err := llm.NewTranscript(sysMsg)
		if err != nil {
			return nil, err
		}
		a.Tr = tr
		sess, err := store.Create("", cwd, providerName, modelID)
		if err != nil {
			return nil, err
		}
		a.Sess = sess
		// Persist the leading system message so the file is complete.
		if err := a.persist(session.MsgEntry(sysMsg)); err != nil {
			return nil, err
		}
	}

	agentCfg := agent.Config{
		Provider: p,
		Model:    a.Model,
		Stream: llm.StreamOptions{
			PromptCacheKey: a.Sess.Header().ID,
			ThinkingLevel:  resolveThinking(settings, opts.ThinkingLevel),
		},
		Tools: registry,
		Env:   config.ShellEnv(a.Sess.Header().ID, providerName, modelID),
		CWD:   cwd,
	}
	a.Agent = agent.New(agentCfg)
	return a, nil
}

func apiShape(providerName string) string {
	if providerName == "anthropic" {
		return "anthropic-messages"
	}
	return "openai-completions"
}

// resolveThinking: flag > settings default > provider default (empty).
func resolveThinking(s *config.Settings, flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return s.DefaultThinkingLevel
}

// persist appends one entry to the session file and the in-memory
// mirror.
func (a *App) persist(e session.Entry) error {
	if err := a.Sess.AppendEntry(e); err != nil {
		return err
	}
	a.entries = append(a.entries, e)
	return nil
}

// Run executes one prompt under ctx (cancellation aborts the run). The
// run's new messages persist as a delta; afterwards compaction fires if
// the projected context exceeds the threshold.
func (a *App) Run(ctx context.Context, out chan<- agent.Event, promptText string) error {
	before := len(a.Tr.Messages())
	err := a.Agent.Prompt(ctx, a.Tr, promptText, out)

	for _, m := range a.Tr.Messages()[before:] {
		if perr := a.persist(session.MsgEntry(m)); perr != nil {
			return perr
		}
	}
	if err != nil {
		return err
	}
	a.maybeCompact(ctx)
	return nil
}

// maybeCompact summarizes the conversation when the projected context
// exceeds the token threshold, appends the marker, and rebuilds the
// projection. Failures skip this round (compaction retries naturally on
// the next prompt).
func (a *App) maybeCompact(ctx context.Context) {
	if !session.NeedsCompaction(a.entries, a.compactTokens) {
		return
	}
	summary, ops, err := a.Agent.Compact(ctx, a.Tr, a.fileOps)
	if err != nil {
		fmt.Fprintf(os.Stderr, "compaction skipped: %v\n", err)
		return
	}
	a.fileOps = ops
	marker := session.NewCompaction(summary, ops.Read, ops.Modified)
	if err := a.persist(session.Entry{Compaction: &marker}); err != nil {
		fmt.Fprintf(os.Stderr, "compaction persist failed: %v\n", err)
		return
	}
	msgs := session.Project(a.entries)
	tr, err := llm.NewTranscript(msgs[0], msgs[1:]...)
	if err == nil {
		a.Tr = tr
	}
}

// Close flushes state.
func (a *App) Close() error {
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
