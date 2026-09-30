package tui

import (
	"net/http"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"scode/internal/cli"
	"scode/internal/llm"
)

// pumpUntil drains UI messages through Update until keep reports done
// or the timeout fires (the TestSubmitRunFlow loop, factored out).
func pumpUntil(t *testing.T, m *model, ui <-chan any, keep func(any) bool) {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for {
		select {
		case msg := <-ui:
			tm, _ := m.Update(msg)
			*m = tm.(model)
			_ = m.View() // render every state like the program loop would
			if keep(msg) {
				return
			}
		case <-timeout:
			t.Fatal("timeout waiting for run completion")
		}
	}
}

func lastUserText(t *testing.T, app *cli.App) string {
	t.Helper()
	msgs := app.Tr.Messages()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleUser {
			return msgs[i].Content[0].Text
		}
	}
	t.Fatal("no user message in transcript")
	return ""
}

func blocksText(m *model) string {
	var sb strings.Builder
	for _, b := range m.blocks {
		sb.WriteString(stripANSI(b.rendered) + "\n")
	}
	return sb.String()
}

// /commit is a prompt, not a UI command: submitting it echoes the turn
// and starts a run whose prompt is the expanded workflow.
func TestPromptCommandSubmitStartsRun(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		chatSSE(w, "committed")
	})
	ui := make(chan any, 256)
	m := newModel(app, ui)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(model)

	m.input.SetValue("/commit focus the message on perf")
	tm, _ = m.submit()
	m = tm.(model)
	if !m.running {
		t.Fatal("/commit did not start a run")
	}
	pumpUntil(t, &m, ui, func(msg any) bool {
		_, ok := msg.(runDoneMsg)
		return ok
	})
	if m.running {
		t.Fatal("still running after runDoneMsg")
	}
	if txt := blocksText(&m); !strings.Contains(txt, "✨ /commit focus the message on perf") {
		t.Fatalf("turn echo missing:\n%s", txt)
	}
	if txt := lastUserText(t, app); !strings.Contains(txt, "Create a git commit for the current changes.") {
		t.Fatalf("prompt command not expanded in the sent prompt:\n%s", txt[:min(200, len(txt))])
	}
}

// Mid-run, /commit queues like every other command (a canned workflow
// must not steer the in-flight turn); when the run finishes, the replay
// starts it as a fresh turn.
func TestPromptCommandQueuedMidRunReplayedAfter(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		chatSSE(w, "reply")
	})
	ui := make(chan any, 256)
	m := newModel(app, ui)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(model)

	m.input.SetValue("hi")
	tm, _ = m.submit()
	m = tm.(model)
	if !m.running {
		t.Fatal("first run did not start")
	}
	m.input.SetValue("/commit")
	tm, _ = m.submit()
	m = tm.(model)
	if len(m.queued) != 1 || m.queued[0] != "/commit" {
		t.Fatalf("mid-run /commit not queued: %v", m.queued)
	}

	// First runDoneMsg: the replay must start the queued /commit turn.
	pumpUntil(t, &m, ui, func(msg any) bool {
		_, ok := msg.(runDoneMsg)
		return ok && m.running
	})
	if len(m.queued) != 0 {
		t.Fatalf("queued /command not consumed by the replay: %v", m.queued)
	}
	// Second runDoneMsg: the /commit turn itself completes.
	pumpUntil(t, &m, ui, func(msg any) bool {
		_, ok := msg.(runDoneMsg)
		return ok
	})
	if m.running {
		t.Fatal("still running after the queued turn")
	}
	txt := blocksText(&m)
	if strings.Count(txt, "✨ /commit") != 1 {
		t.Fatalf("queued turn echo missing:\n%s", txt)
	}
	if body := lastUserText(t, app); !strings.Contains(body, "Create a git commit") {
		t.Fatalf("queued /commit ran unexpanded:\n%s", body[:min(200, len(body))])
	}
}

// Commands queued BEHIND a prompt command wait for that turn instead of
// firing while it is in flight.
func TestQueuedCommandWaitsBehindPromptCommand(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		chatSSE(w, "reply")
	})
	ui := make(chan any, 256)
	m := newModel(app, ui)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(model)

	m.input.SetValue("hi")
	tm, _ = m.submit()
	m = tm.(model)
	for _, c := range []string{"/commit", "/cost"} {
		m.input.SetValue(c)
		tm, _ = m.submit()
		m = tm.(model)
	}

	// First runDoneMsg: replay starts /commit and re-queues /cost.
	pumpUntil(t, &m, ui, func(msg any) bool {
		_, ok := msg.(runDoneMsg)
		return ok
	})
	if !m.running {
		t.Fatal("queued /commit did not start a turn")
	}
	if len(m.queued) != 1 || m.queued[0] != "/cost" {
		t.Fatalf("/cost should wait behind the /commit turn: %v", m.queued)
	}
	pumpUntil(t, &m, ui, func(msg any) bool {
		_, ok := msg.(runDoneMsg)
		return ok
	})
	if m.running {
		t.Fatal("still running")
	}
	if len(m.queued) != 0 {
		t.Fatalf("/cost never drained: %v", m.queued)
	}
	if txt := blocksText(&m); !strings.Contains(txt, "上下文:") {
		t.Fatalf("/cost output missing after drain:\n%s", truncate(txt, 300))
	}
}

// The palette lists the prompt commands as first-class entries.
func TestPaletteListsPromptCommands(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		chatSSE(w, "reply")
	})
	m := newModel(app, make(chan any, 4))
	m.input.SetValue("/")
	m.updatePalette()
	found := map[string]string{}
	for _, h := range m.paletteHits {
		found[h.Name] = h.Desc
	}
	if d, ok := found["/commit"]; !ok || !strings.Contains(d, "git commit") {
		t.Fatalf("/commit missing from palette: %v", found)
	}
	if d, ok := found["/commit-push-pr"]; !ok || !strings.Contains(d, "PR") {
		t.Fatalf("/commit-push-pr missing from palette: %v", found)
	}
}
