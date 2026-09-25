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
type App struct {
	CWD      string
	CfgDir   string
	Provider llm.Provider
	Model    llm.Model
	Settings *config.Settings

	Agent     *agent.Agent
	Store     *session.Store
	Sess      *session.Session
	Tr        *llm.Transcript
	persisted int
}

// Options select provider/model/session at startup.
type Options struct {
	Provider string
	Model    string
	Resume   string // session id
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
		tr, err := rec.Transcript()
		if err != nil {
			return nil, err
		}
		a.Tr = tr
		a.persisted = len(rec.Messages)
		sess, err := store.Create(rec.Header.ID+"-r", cwd, providerName, modelID)
		if err != nil {
			return nil, err
		}
		a.Sess = sess
	} else {
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
		if err := sess.Append(sysMsg); err != nil {
			return nil, err
		}
		a.persisted = 1
	}

	agentCfg := agent.Config{
		Provider: p,
		Model:    a.Model,
		Stream:   llm.StreamOptions{PromptCacheKey: a.Sess.Header().ID},
		Tools:    registry,
		Env:      config.ShellEnv(a.Sess.Header().ID, providerName, modelID),
		CWD:      cwd,
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

// persistTail writes every not-yet-persisted transcript message to the
// session file (after each completed run — crash window is one run).
func (a *App) persistTail() error {
	msgs := a.Tr.Messages()
	for i := a.persisted; i < len(msgs); i++ {
		if err := a.Sess.Append(msgs[i]); err != nil {
			return err
		}
	}
	a.persisted = len(msgs)
	return nil
}

// Close flushes state.
func (a *App) Close() error {
	_ = a.persistTail()
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

// Run executes one prompt under ctx (cancellation aborts the run).
func (a *App) Run(ctx context.Context, out chan<- agent.Event, promptText string) error {
	err := a.Agent.Prompt(ctx, a.Tr, promptText, out)
	_ = a.persistTail()
	return err
}
