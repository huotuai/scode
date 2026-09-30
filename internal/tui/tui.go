// Package tui is scode's interactive terminal UI (bubbletea v2). It is
// a pure frontend: all session logic lives in cli.App, approvals flow
// through the cli.AskReviewer seam, and agent events stream into the
// bubbletea loop over one channel.
package tui

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"scode/internal/cli"
)

// Run starts the interactive TUI session. stderr is captured into the
// transcript so diagnostics (sandbox/MCP/skills notices, compaction
// warnings) render in place instead of corrupting the alt screen.
func Run(opts cli.Options) (err error) {
	ui := make(chan any, 256)
	restore := captureStderr(ui)
	defer restore()

	opts.Approver = newApprover(ui)
	app, err := cli.Setup(opts)
	if err != nil {
		return err
	}
	app.MarkInteractive() // interactive surface: close-time memory extraction applies
	defer app.Close()     //nolint:errcheck

	// Crash visibility: bubbletea's catchPanics turns a model panic into
	// the p.Run() error, but after the alt screen exits that message is
	// easy to lose (double-clicked window closes instantly). Persist it.
	defer func() {
		if r := recover(); r != nil {
			writeCrashLog(app.CfgDir, fmt.Sprintf("panic: %v\n%s", r, debug.Stack()))
			err = fmt.Errorf("tui panic: %v (details: %s)", r, crashLogPath(app.CfgDir))
		}
	}()

	p := tea.NewProgram(newModel(app, ui))
	if _, rerr := p.Run(); rerr != nil {
		writeCrashLog(app.CfgDir, "run error: "+rerr.Error())
		return fmt.Errorf("tui: %w (details: %s)", rerr, crashLogPath(app.CfgDir))
	}
	// The alt screen is gone now — the farewell hint names the session
	// and how to resume it, right where the user lands.
	fmt.Println(app.ResumeHint())
	return nil
}

func crashLogPath(cfgDir string) string {
	return filepath.Join(cfgDir, "tui-crash.log")
}

func writeCrashLog(cfgDir, text string) {
	f, err := os.OpenFile(crashLogPath(cfgDir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck
	fmt.Fprintf(f, "\n== %s ==\n%s\n", time.Now().Format(time.RFC3339), text)
}

// captureStderr redirects process stderr into the UI event stream.
// Lines written before the program starts sit in the buffered channel
// and render once the loop is up.
func captureStderr(ui chan<- any) (restore func()) {
	r, w, err := os.Pipe()
	if err != nil {
		return func() {}
	}
	old := os.Stderr
	os.Stderr = w
	var once sync.Once
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
		for sc.Scan() {
			ui <- logMsg(sc.Text())
		}
	}()
	return func() {
		once.Do(func() {
			os.Stderr = old
			w.Close() //nolint:errcheck
		})
	}
}
