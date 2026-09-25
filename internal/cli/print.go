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

// REPL is the interactive line mode.
func REPL(opts Options) error {
	app, err := Setup(opts)
	if err != nil {
		return err
	}
	defer app.Close()

	fmt.Printf("scode (%s / %s) — session %s\n/exit to quit, /sessions to list\n\n",
		app.Model.Provider, app.Model.ID, app.Sess.Header().ID)

	r := &Renderer{Out: os.Stdout, Err: os.Stderr}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	ctx, cancel := abortableCtx()
	defer cancel()
	watchCancel(cancel)

	for {
		fmt.Print("> ")
		if !sc.Scan() {
			fmt.Println()
			return nil
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if line[0] == '/' {
			if done, err := app.command(line); done || err != nil {
				return err
			}
			continue
		}
		if err := runPrompt(ctx, app, r, line); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
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
			if id+"-r" == a.Sess.Header().ID {
				mark = " *"
			}
			fmt.Println("  " + id + mark)
		}
		return false, nil
	case line == "/model":
		fmt.Printf("%s / %s\n", a.Model.Provider, a.Model.ID)
		return false, nil
	case len(line) > 8 && line[:8] == "/resume ":
		fmt.Fprintln(os.Stderr, "resume needs a restart: scode --resume <id>")
		return false, nil
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", line)
		return false, nil
	}
}
