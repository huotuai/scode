// Command scode is a lean Go coding agent built on pi's architecture.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"scode/internal/cli"
	"scode/internal/config"
	"scode/internal/i18n"
	"scode/internal/sandbox"
	"scode/internal/server"
	"scode/internal/tui"
	"scode/internal/update"
)

var version = "dev"

// stringList collects a repeatable flag (pi's --skill <path>).
type stringList []string

func (s *stringList) String() string { return fmt.Sprint([]string(*s)) }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	// Sandbox launcher self-re-exec (linux landlock rung): the child must
	// install the ruleset before any other initialization runs.
	if sandbox.RunLauncherCommand() {
		return
	}
	// Remove the <exe>.old a previous `scode update` left behind (the
	// old image can only be deleted once it is no longer running).
	update.CleanStale()
	// `scode serve`: headless JSON-RPC session service on stdio (the
	// desktop client's backend; design: docs/design-permission-plan-desktop.md).
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		if err := server.Run(context.Background(), os.Stdin, os.Stdout, server.AutoTitle()); err != nil {
			fmt.Fprintln(os.Stderr, "serve:", err)
			os.Exit(1)
		}
		return
	}
	// `scode update`: self-update from the configured GitHub release
	// source (-check only reports, -force reinstalls the current tag).
	if len(os.Args) > 1 && os.Args[1] == "update" {
		if err := runUpdate(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "update:", err)
			os.Exit(1)
		}
		return
	}
	// `scode version`: version plus latest-release info (unlike the
	// script-friendly --version, which prints the bare string).
	if len(os.Args) > 1 && os.Args[1] == "version" {
		runVersion()
		return
	}

	fs := flag.NewFlagSet("scode", flag.ContinueOnError)
	printMode := fs.Bool("p", false, "one-shot: run prompt(s) and exit (auto when stdout is not a TTY)")
	printLong := fs.Bool("print", false, "same as -p")
	provider := fs.String("provider", "", "provider: anthropic | openai-compat | openai-responses | azure-openai-responses | google")
	model := fs.String("model", "", "model id")
	thinking := fs.String("thinking", "", "reasoning intensity: off | low | medium | high")
	resume := fs.String("resume", "", "resume a session id")
	var skillPaths stringList
	fs.Var(&skillPaths, "skill", "load a skill file or directory (repeatable)")
	noSkills := fs.Bool("no-skills", false, "disable skills discovery and loading (--skill paths still load)")
	replMode := fs.Bool("repl", false, "use the legacy line REPL instead of the TUI")
	planMode := fs.Bool("plan", false, "start the session in plan mode (read-only research, plan review gate)")
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

	opts := cli.Options{Provider: *provider, Model: *model, Resume: *resume, ThinkingLevel: *thinking, SkillPaths: skillPaths, NoSkills: *noSkills, Plan: *planMode}

	if *sessions {
		if err := cli.ListSessions(opts); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	prompts := fs.Args()
	oneShot := !*replMode && (*printMode || *printLong || !isTTY(os.Stdout) || len(prompts) > 0)

	// Interactive sessions only: surface a cached new-version notice and
	// refresh the cache in the background when stale (never blocking,
	// never in one-shot/serve mode where stdout is a contract).
	if !oneShot && !*replMode {
		maybeNotifyUpdate()
	}

	var err error
	if oneShot {
		if len(prompts) == 0 {
			fmt.Fprintln(os.Stderr, "error: -p requires a prompt")
			os.Exit(2)
		}
		err = cli.Print(opts, prompts)
	} else if *replMode || !isTTY(os.Stdin) {
		// --repl forces the line REPL; a non-TTY stdin (piped input)
		// falls back to it since the TUI needs the terminal.
		err = cli.REPL(opts)
	} else {
		err = tui.Run(opts)
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

// maybeNotifyUpdate prints the cached new-version hint when fresh and
// kicks a background cache refresh otherwise. Disabled via settings
// updateCheck=false or when no release source is configured.
func maybeNotifyUpdate() {
	settings, err := config.LoadSettings()
	if err != nil || !settings.UpdateCheckOn() {
		return
	}
	repo := settings.ResolveUpdateRepo(update.DefaultRepo)
	if repo == "" {
		return
	}
	dir, err := config.Dir()
	if err != nil {
		return
	}
	if res, ok := update.CheckCached(dir, version); ok {
		if res.Newer {
			fmt.Fprintln(os.Stderr, res.Notice())
		}
		return
	}
	go update.RefreshCache(repo, dir)
}

// runUpdate implements `scode update [-check] [-force]`.
func runUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	checkOnly := fs.Bool("check", false, "only check for a new version")
	force := fs.Bool("force", false, "reinstall even when up to date")
	if err := fs.Parse(args); err != nil {
		return err
	}
	settings, err := config.LoadSettings()
	if err != nil {
		return err
	}
	repo := settings.ResolveUpdateRepo(update.DefaultRepo)
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	ctx := context.Background()
	if *checkOnly {
		res, err := update.CheckNow(ctx, repo, dir, version)
		if err != nil {
			return err
		}
		if res.Newer {
			fmt.Println(res.Notice())
		} else {
			fmt.Println(i18n.Tf("update.uptodate", version, res.Tag))
		}
		return nil
	}
	fmt.Println(i18n.Tf("update.checking", repo))
	res, err := update.SelfUpdate(ctx, repo, dir, version, *force)
	if err != nil {
		return err
	}
	if !res.Newer && !*force {
		fmt.Println(i18n.Tf("update.uptodate", version, res.Tag))
		return nil
	}
	fmt.Println(i18n.Tf("update.done", res.Tag))
	return nil
}

// runVersion implements `scode version`.
func runVersion() {
	fmt.Println("scode", version)
	settings, err := config.LoadSettings()
	if err != nil {
		return
	}
	repo := settings.ResolveUpdateRepo(update.DefaultRepo)
	if repo == "" {
		return
	}
	dir, err := config.Dir()
	if err != nil {
		return
	}
	res, err := update.CheckNow(context.Background(), repo, dir, version)
	if err != nil {
		return // offline: the bare version line is the contract
	}
	if res.Newer {
		fmt.Println(res.Notice())
	} else {
		fmt.Println(i18n.T("version.uptodate"))
	}
}
