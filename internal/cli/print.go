package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"scode/internal/agent"
	"scode/internal/config"
	"scode/internal/i18n"
)

// runPrompt drives one prompt to completion, streaming events through
// the renderer. Returns the run error.
func runPrompt(ctx context.Context, app *App, r *Renderer, promptText string) error {
	out := make(chan agent.Event, 256)
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, out, promptText) }()
	for ev := range out {
		r.Handle(ev)
	}
	return <-done
}

// runPromptInteractive additionally accepts lines from input while the
// run is in flight: plain lines steer the agent, slash commands are
// held back and returned so the REPL can run them once the turn ends
// (sending "/cost" to the model as a correction would be nonsense).
func runPromptInteractive(ctx context.Context, app *App, r *Renderer, promptText string, input <-chan string) ([]string, error) {
	out := make(chan agent.Event, 256)
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, out, promptText) }()
	var pending []string
	for out != nil {
		select {
		case ev, ok := <-out:
			if !ok {
				out = nil
				continue
			}
			r.Handle(ev)
		case line, ok := <-input:
			if !ok {
				input = nil // stdin closed: disable the case instead of spinning
				continue
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			// A pending approval owns the next y/n/a/p line; anything
			// else falls through to steering/commands as usual.
			if app.approver.Route(line) {
				continue
			}
			if strings.HasPrefix(line, "/") && !strings.HasPrefix(line, "/skill:") {
				pending = append(pending, line)
				fmt.Fprintf(r.Err, "(queued command for after this run: %s)\n", line)
				continue
			}
			// Steering expands /skill:name too (pi's _queueUserInput →
			// _expandSkillCommand).
			app.Steer(line)
			fmt.Fprintf(r.Err, "(steered: %s)\n", truncateFor(line, 60))
		}
	}
	return pending, <-done
}

func truncateFor(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// abortableCtx returns a ctx cancelled by the first Ctrl-C (a second
// one exits hard) plus a stop func releasing the signal watcher. Create
// one PER RUN: an abort kills the in-flight run, never the session.
func abortableCtx() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sig:
			cancel()
			fmt.Fprintln(os.Stderr, "\n(aborted — Ctrl-C again to exit)")
			select {
			case <-sig:
				os.Exit(130)
			case <-done:
			}
		case <-done:
		}
	}()
	return ctx, func() { signal.Stop(sig); close(done); cancel() }
}

// Print runs prompts one-shot: assistant text to stdout, tool activity
// and errors to stderr; exit code 1 on failure (pi's print mode shape).
func Print(opts Options, prompts []string) error {
	app, err := Setup(opts)
	if err != nil {
		return err
	}
	defer app.Close()

	r := &Renderer{Out: os.Stdout, Err: os.Stderr}
	ctx, stop := abortableCtx() // one-shot: an abort fails the whole batch
	defer stop()

	failed := false
	for _, p := range prompts {
		if err := runPrompt(ctx, app, r, p); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			failed = true
		}
	}
	if failed {
		return ErrReported
	}
	return nil
}

// ErrReported marks "already printed to stderr, just exit non-zero".
var ErrReported = errors.New("reported")

// ResumeHint is the farewell line printed when an interactive session
// (REPL or TUI) ends: the session id plus how to pick it back up.
func (a *App) ResumeHint() string {
	id := a.Sess.Header().ID
	return i18n.Tf("cli.print.sessionSaved", id, id)
}

// REPL is the interactive line mode. A single reader goroutine owns
// stdin; while a run is in flight its lines steer the agent instead of
// queuing a new prompt.
func REPL(opts Options) error {
	app, err := Setup(opts)
	if err != nil {
		return err
	}
	defer app.Close()
	// Runs before the Close defer (LIFO): the farewell hint names the
	// session and how to resume it — printed on every exit path.
	defer func() { fmt.Println("\n" + app.ResumeHint()) }()
	app.approver.interactive = true // REPL has an input loop to answer approval prompts
	app.interactive = true          // ... and is an interactive surface (memory auto-extraction)

	fmt.Printf("scode (%s / %s) — session %s\n/exit to quit, /sessions to list, /reload to rescan skills\n(type during a run to steer it)\n\n",
		app.Model.Provider, app.Model.ID, app.Sess.Header().ID)

	r := &Renderer{Out: os.Stdout, Err: os.Stderr}
	input := make(chan string)
	go func() {
		defer close(input)
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
		for sc.Scan() {
			input <- sc.Text()
		}
	}()

	for {
		if app.planCtl.Active() {
			fmt.Print("[plan]> ")
		} else {
			fmt.Print("> ")
		}
		line, ok := <-input
		if !ok {
			fmt.Println()
			return nil
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// /skill:name and the built-in prompt commands (/commit, ...) are
		// NOT REPL commands — they flow to the prompt path and expand
		// there (pi's agent-session.prompt). Every other "/" line is a
		// REPL command.
		if line[0] == '/' && !strings.HasPrefix(line, "/skill:") && !IsPromptCommand(line) {
			out, done, err := app.Command(line)
			if out != "" {
				fmt.Println(out)
			}
			if done || err != nil {
				return err
			}
			continue
		}
		// Fresh ctx per run: a Ctrl-C aborts this run, and the REPL
		// stays usable afterwards (pi's per-run AbortController).
		ctx, stop := abortableCtx()
		pending, err := runPromptInteractive(ctx, app, r, line, input)
		stop()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		// Commands queued mid-run replay now; a queued prompt command
		// (/commit typed during another run) starts a turn of its own,
		// and anything queued during THAT turn joins the back of the
		// queue (drained by index, not range, so the appends are seen).
		for len(pending) > 0 {
			cmd := pending[0]
			pending = pending[1:]
			if !IsPromptCommand(cmd) {
				out, done, cerr := app.Command(cmd)
				if out != "" {
					fmt.Println(out)
				}
				if done || cerr != nil {
					return cerr
				}
				continue
			}
			ctx, stop := abortableCtx()
			more, rerr := runPromptInteractive(ctx, app, r, cmd, input)
			stop()
			if rerr != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", rerr)
			}
			pending = append(pending, more...)
		}
	}
}

// Steer forwards a mid-run user line to the agent, expanding
// /skill:name like the prompt path does (pi's _queueUserInput).
func (a *App) Steer(line string) {
	a.Agent.Steer(a.expandSkill(line))
}

// Command runs one slash command and returns the text to display;
// done=true ends the session UI (/exit). Shared by the line REPL and
// the TUI.
func (a *App) Command(line string) (out string, done bool, err error) {
	switch {
	case line == "/plan" || strings.HasPrefix(line, "/plan "):
		arg := strings.TrimSpace(strings.TrimPrefix(line, "/plan"))
		if arg == "off" {
			if a.planCtl.Set(false) {
				return "plan mode off", false, nil
			}
			return "(plan mode is already inactive)", false, nil
		}
		if a.planCtl.Set(true) {
			return "plan mode on — read-only research, then the model submits a plan for your review; /plan off leaves directly", false, nil
		}
		return "(plan mode is already active)", false, nil
	case line == "/exit" || line == "/quit":
		return "", true, nil
	case line == "/reload":
		// Hot reload: re-run skill discovery mid-session so newly added
		// skills expand and list without a restart.
		return "skills: " + a.ReloadSkills(), false, nil
	case line == "/mcp":
		return a.mcpListText(), false, nil
	case line == "/agents":
		return a.AgentListText(), false, nil
	case line == "/config":
		return a.ConfigListText(), false, nil
	case line == "/rewind":
		return a.CheckpointListText(), false, nil
	case strings.HasPrefix(line, "/rewind "):
		return a.CheckpointRestoreText(strings.TrimPrefix(line, "/rewind ")), false, nil
	case line == "/memory":
		return a.MemoryListText(), false, nil
	case line == "/memory extract":
		summary, err := a.MemoryExtract(context.Background())
		if err != nil {
			return "memory: " + err.Error(), false, nil
		}
		return summary + "\n" + a.MemoryListText(), false, nil
	case line == "/sessions":
		ids, err := a.Store.List()
		if err != nil {
			return "error: " + err.Error(), false, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "sessions in %s:\n", a.Store.Root)
		for _, id := range ids {
			mark := ""
			if id == a.Sess.Header().ID {
				mark = " *"
			}
			fmt.Fprintln(&b, "  "+id+mark)
		}
		return strings.TrimRight(b.String(), "\n"), false, nil
	case line == "/model":
		return i18n.Tf("cli.print.modelLine", a.Model.Provider, a.Model.ID), false, nil
	case strings.HasPrefix(line, "/model "):
		q := strings.TrimSpace(strings.TrimPrefix(line, "/model"))
		choices := a.ConfiguredModels()
		// Exact provider / model / label first, then a case-insensitive
		// label substring.
		match := -1
		for i, c := range choices {
			if c.Provider == q || c.Model == q || c.Label == q {
				match = i
				break
			}
		}
		if match < 0 {
			for i, c := range choices {
				if strings.Contains(strings.ToLower(c.Label), strings.ToLower(q)) {
					match = i
					break
				}
			}
		}
		if match < 0 {
			var b strings.Builder
			fmt.Fprintln(&b, "no configured model matches "+strconv.Quote(q)+"; configured:")
			for _, c := range choices {
				fmt.Fprintln(&b, "  "+c.Label)
			}
			return strings.TrimRight(b.String(), "\n"), false, nil
		}
		p, mid, err := a.SetModel(choices[match].Provider, choices[match].Model)
		if err != nil {
			return "model switch failed: " + err.Error(), false, nil
		}
		// The switch also becomes the settings-level default, so new
		// sessions and the next launch start from it.
		if derr := a.PersistDefaultModel(p, mid); derr != nil {
			return fmt.Sprintf("model → %s / %s (default save failed: %v)", p, mid, derr), false, nil
		}
		return fmt.Sprintf("model → %s / %s (saved as default)", p, mid), false, nil
	case line == "/models":
		// The interactive four-stage configuration (vendor → key → model
		// → confirm) is a TUI overlay; the REPL prints the preset catalog
		// so its contents are still inspectable here.
		cat, err := config.LoadPresetCatalog()
		if err != nil {
			return i18n.Tf("tui.models.catalogFail", err), false, nil
		}
		var b strings.Builder
		fmt.Fprintln(&b, i18n.T("cli.print.catalogHeader"))
		for _, v := range cat.Vendors {
			fmt.Fprintf(&b, "%s (%s)\n", v.Name, v.Protocol)
			for _, pm := range v.Models {
				spec := i18n.Tf("cli.print.spec", pm.ContextWindow, pm.MaxTokens)
				if pm.Reasoning {
					var lv []string
					for _, e := range pm.Efforts {
						lv = append(lv, e.Level)
					}
					spec += i18n.Tf("cli.print.specReasoning", strings.Join(lv, "/"))
				}
				fmt.Fprintf(&b, "  %s — %s\n", pm.ID, spec)
			}
		}
		return strings.TrimRight(b.String(), "\n"), false, nil
	case line == "/cost":
		return a.CostReport(), false, nil
	case line == "/compact" || strings.HasPrefix(line, "/compact "):
		custom := strings.TrimSpace(strings.TrimPrefix(line, "/compact"))
		err := a.compactNow(context.Background(), custom)
		if errors.Is(err, ErrNothingToCompact) {
			return "(nothing to compact — history already fits in the kept tail)", false, nil
		}
		if err != nil {
			return "compact failed: " + err.Error(), false, nil
		}
		return "compacted — context rebuilt: summary + recent verbatim tail", false, nil
	case line == "/fork":
		id, err := a.Store.Fork(a.Sess.Header().ID, 0)
		if err != nil {
			return "fork failed: " + err.Error(), false, nil
		}
		return fmt.Sprintf("forked to %s — restart with: scode --resume %s", id, id), false, nil
	case strings.HasPrefix(line, "/fork "):
		n := 0
		if _, err := fmt.Sscanf(line, "/fork %s %d", new(string), &n); err != nil || n <= 0 {
			return "usage: /fork [entryCount] — clone this session (optionally keeping only the first N entries)", false, nil
		}
		id, err := a.Store.Fork(a.Sess.Header().ID, n)
		if err != nil {
			return "fork failed: " + err.Error(), false, nil
		}
		return fmt.Sprintf("forked (first %d entries) to %s — restart with: scode --resume %s", n, id, id), false, nil
	case line == "/new":
		// The TUI intercepts /new before App.Command (it also resets the
		// transcript view); this is the REPL surface — the session swaps
		// in place and the notice names both ids.
		oldID, newID, err := a.NewSession()
		if err != nil {
			return "new session failed: " + err.Error(), false, nil
		}
		return i18n.Tf("cli.print.newSession", newID, oldID), false, nil
	case len(line) > 8 && line[:8] == "/resume ":
		return "resume needs a restart: scode --resume <id>", false, nil
	default:
		return "unknown command: " + line, false, nil
	}
}
