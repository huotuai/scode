package cli

// Code rewind: edit/write executions are wrapped with a BEFORE-state
// snapshot (internal/checkpoint), so /rewind can roll files back to any
// AI edit point. bash is deliberately NOT wrapped — its file effects
// are opaque up front; the deterministic editor tools carry the
// feature. The rewindCheckpoints /config switch gates capture (read at
// setup; a mid-session toggle applies on the next launch).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"scode/internal/agent"
	"scode/internal/checkpoint"
	"scode/internal/i18n"
	"scode/internal/llm"
)

// snapTool wraps an editing tool: capture the target file's before
// state, delegate, and drop the checkpoint again when the call failed
// (nothing changed — the point would be noise).
type snapTool struct {
	name    string
	inner   agent.Tool
	store   *checkpoint.Store
	session func() string
}

func (t snapTool) Decl() llm.Tool { return t.inner.Decl() }

func (t snapTool) Execute(tc agent.ToolContext, args json.RawMessage) agent.ToolResult {
	var a struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(args, &a) // best effort: no path, no snapshot
	if p := strings.TrimSpace(a.Path); p != "" {
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(tc.CWD, abs)
		}
		cp, err := t.store.Capture(t.session(), t.name, filepath.Base(abs), []string{abs})
		if err != nil {
			fmt.Fprintf(os.Stderr, "checkpoint: %v\n", err)
		} else {
			res := t.inner.Execute(tc, args)
			if res.IsError {
				t.store.Remove(t.session(), cp.ID)
			}
			return res
		}
	}
	return t.inner.Execute(tc, args)
}

// wrapCheckpoints installs the snapshot decorators over edit/write
// (same-name Add replaces the originals; declarations are delegated, so
// the system prompt never changes shape).
func (a *App) wrapCheckpoints(registry *agent.Registry) {
	sess := func() string {
		if a.Sess != nil {
			return a.Sess.Header().ID
		}
		return "unknown"
	}
	for _, name := range []string{"edit", "write"} {
		if tool, ok := registry.Get(name); ok {
			registry.Add(snapTool{name: name, inner: tool, store: a.checkpoints, session: sess})
		}
	}
}

// Checkpoints lists this session's rewind points, newest first.
func (a *App) Checkpoints() []checkpoint.Checkpoint {
	return a.checkpoints.List(a.Sess.Header().ID)
}

// CheckpointRestore rolls files back to one checkpoint (host-side
// writes: an explicit user command, not an agent tool call).
func (a *App) CheckpointRestore(id string) (int, error) {
	return a.checkpoints.Restore(a.Sess.Header().ID, id)
}

// CheckpointListText renders the /rewind listing (REPL surface).
func (a *App) CheckpointListText() string {
	cps := a.Checkpoints()
	if len(cps) == 0 {
		return i18n.T("cli.checkpoint.none")
	}
	rows := make([]string, 0, len(cps))
	for _, cp := range cps {
		files := i18n.T("tui.rewind.oneFile")
		if len(cp.Files) != 1 {
			files = i18n.Tf("tui.rewind.nFiles", len(cp.Files))
		}
		if n := tooLargeCount(cp); n > 0 {
			files += i18n.Tf("tui.rewind.tooLarge", n)
		}
		rows = append(rows, fmt.Sprintf("  %s · %s · %s · %s", cp.ID, cp.Time.Format("15:04:05"), cp.Tool, cp.Summary+" ("+files+")"))
	}
	return i18n.Tf("cli.checkpoint.list", strings.Join(rows, "\n"))
}

// tooLargeCount counts the checkpoint's over-cap (unarchived) files.
func tooLargeCount(cp checkpoint.Checkpoint) int {
	n := 0
	for _, f := range cp.Files {
		if f.TooLarge {
			n++
		}
	}
	return n
}

// CheckpointRestoreText handles the REPL's /rewind argument form.
func (a *App) CheckpointRestoreText(arg string) string {
	cps := a.Checkpoints()
	if len(cps) == 0 {
		return i18n.T("cli.checkpoint.noneShort")
	}
	id := strings.TrimSpace(arg)
	if id == "last" || id == "" {
		id = cps[0].ID
	}
	n, err := a.CheckpointRestore(id)
	if err != nil {
		return "rewind: " + err.Error()
	}
	return i18n.Tf("cli.checkpoint.restored", id, n)
}
