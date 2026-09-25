// Command scode is a lean Go coding agent built on pi's architecture.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"scode/internal/cli"
)

var version = "dev"

func main() {
	fs := flag.NewFlagSet("scode", flag.ContinueOnError)
	printMode := fs.Bool("p", false, "one-shot: run prompt(s) and exit (auto when stdout is not a TTY)")
	printLong := fs.Bool("print", false, "same as -p")
	provider := fs.String("provider", "", "provider: anthropic | openai-compat")
	model := fs.String("model", "", "model id")
	thinking := fs.String("thinking", "", "reasoning intensity: off | low | medium | high")
	resume := fs.String("resume", "", "resume a session id")
	replMode := fs.Bool("repl", false, "force interactive REPL mode even when stdout is not a TTY")
	sessions := fs.Bool("sessions", false, "list sessions for this directory")
	showVersion := fs.Bool("version", false, "print version")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "scode [flags] [prompt...]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	if *showVersion {
		fmt.Println("scode", version)
		return
	}

	opts := cli.Options{Provider: *provider, Model: *model, Resume: *resume, ThinkingLevel: *thinking}

	if *sessions {
		if err := cli.ListSessions(opts); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	prompts := fs.Args()
	oneShot := !*replMode && (*printMode || *printLong || !isTTY(os.Stdout) || len(prompts) > 0)

	var err error
	if oneShot {
		if len(prompts) == 0 {
			fmt.Fprintln(os.Stderr, "error: -p requires a prompt")
			os.Exit(2)
		}
		err = cli.Print(opts, prompts)
	} else {
		err = cli.REPL(opts)
	}
	if err != nil {
		if !errors.Is(err, cli.ErrReported) {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
	}
}

// isTTY reports whether f is a terminal.
func isTTY(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
