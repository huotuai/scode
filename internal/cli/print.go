package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"scode/internal/agent"
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
				continue
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "/") {
				pending = append(pending, line)
				fmt.Fprintf(r.Err, "(queued command for after this run: %s)\n", line)
				continue
			}
			app.Agent.Steer(line)
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

// abortableCtx cancels on the first Ctrl-C; the caller decides what a
// second one means (exit).
func abortableCtx() (context.Context, func()) {
	return context.WithCancel(context.Background())
}

// watchCancel cancels ctx on SIGINT once; a second SIGINT exits hard.
func watchCancel(cancel func()) {
	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		cancel()
		fmt.Fprintln(os.Stderr, "\n(aborted — Ctrl-C again to exit)")
		<-c
		os.Exit(130)
	}()
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
	ctx, cancel := abortableCtx()
	defer cancel()
	watchCancel(cancel)

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

// REPL is the interactive line mode. A single reader goroutine owns
// stdin; while a run is in flight its lines steer the agent instead of
// queuing a new prompt.
func REPL(opts Options) error {
	app, err := Setup(opts)
	if err != nil {
		return err
	}
	defer app.Close()

	fmt.Printf("scode (%s / %s) — session %s\n/exit to quit, /sessions to list\n(type during a run to steer it)\n\n",
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
	ctx, cancel := abortableCtx()
	defer cancel()
	watchCancel(cancel)

	for {
		fmt.Print("> ")
		line, ok := <-input
		if !ok {
			fmt.Println()
			return nil
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line[0] == '/' {
			if done, err := app.command(line); done || err != nil {
				return err
			}
			continue
		}
		pending, err := runPromptInteractive(ctx, app, r, line, input)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		for _, cmd := range pending {
			if done, cerr := app.command(cmd); done || cerr != nil {
				return cerr
			}
		}
	}
}

// command handles slash commands; returns done=true when the REPL ends.
func (a *App) command(line string) (bool, error) {
	switch {
	case line == "/exit" || line == "/quit":
		return true, nil
	case line == "/sessions":
		ids, err := a.Store.List()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return false, nil
		}
		fmt.Printf("sessions in %s:\n", a.Store.Root)
		for _, id := range ids {
			mark := ""
			if id == a.Sess.Header().ID {
				mark = " *"
			}
			fmt.Println("  " + id + mark)
		}
		return false, nil
	case line == "/model":
		fmt.Printf("%s / %s\n", a.Model.Provider, a.Model.ID)
		return false, nil
	case line == "/cost":
		fmt.Println(a.CostReport())
		return false, nil
	case line == "/fork":
		id, err := a.Store.Fork(a.Sess.Header().ID, 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fork failed:", err)
			return false, nil
		}
		fmt.Printf("forked to %s — restart with: scode --resume %s\n", id, id)
		return false, nil
	case strings.HasPrefix(line, "/fork "):
		n := 0
		if _, err := fmt.Sscanf(line, "/fork %s %d", new(string), &n); err != nil || n <= 0 {
			fmt.Fprintln(os.Stderr, "usage: /fork [entryCount] — clone this session (optionally keeping only the first N entries)")
			return false, nil
		}
		id, err := a.Store.Fork(a.Sess.Header().ID, n)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fork failed:", err)
			return false, nil
		}
		fmt.Printf("forked (first %d entries) to %s — restart with: scode --resume %s\n", n, id, id)
		return false, nil
	case len(line) > 8 && line[:8] == "/resume ":
		fmt.Fprintln(os.Stderr, "resume needs a restart: scode --resume <id>")
		return false, nil
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", line)
		return false, nil
	}
}
