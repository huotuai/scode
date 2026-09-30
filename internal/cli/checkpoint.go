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
		return "暂无检查点 — AI 的 edit/write 修改会自动留档(可用 /config 关闭)"
	}
	rows := make([]string, 0, len(cps))
	for _, cp := range cps {
		files := "1 个文件"
		if len(cp.Files) != 1 {
			files = fmt.Sprintf("%d 个文件", len(cp.Files))
		}
		if n := tooLargeCount(cp); n > 0 {
			files += fmt.Sprintf(",%d 个过大未留档", n)
		}
		rows = append(rows, fmt.Sprintf("  %s · %s · %s · %s", cp.ID, cp.Time.Format("15:04:05"), cp.Tool, cp.Summary+" ("+files+")"))
	}
	return "检查点 (新→旧):\n" + strings.Join(rows, "\n") +
		"\n/rewind <id> 或 /rewind last 恢复文件到该时点(仅回滚文件,不回滚对话)"
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
		return "暂无检查点"
	}
	id := strings.TrimSpace(arg)
	if id == "last" || id == "" {
		id = cps[0].ID
	}
	n, err := a.CheckpointRestore(id)
	if err != nil {
		return "rewind: " + err.Error()
	}
	return fmt.Sprintf("已回滚到检查点 %s · 恢复 %d 个文件(对话不受影响)", id, n)
}
