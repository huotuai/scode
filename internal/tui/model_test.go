package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"scode/internal/agent"
	"scode/internal/cli"
	"scode/internal/llm"
	"scode/internal/permission"
)

func newTestModel() model {
	ta := textarea.New()
	ta.SetWidth(80)
	return model{
		vp:           scroller{height: 20, pinned: true},
		input:        ta,
		width:        80,
		height:       24,
		contentWidth: 80,
		ui:           make(chan any, 16),
	}
}

// Regression: the input must be focused after newModel — focusing in
// Init() would mutate a discarded copy (value receiver), leaving the
// textarea blind to keystrokes and cursorless.
func TestNewModelInputFocused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	cfgDir := t.TempDir()
	proj := t.TempDir()
	settings := map[string]any{
		"defaultProvider": "openai-compat",
		"providers": map[string]any{
			"openai-compat": map[string]any{"apiKey": "k", "baseUrl": srv.URL, "model": "m"},
		},
	}
	sb, _ := json.Marshal(settings)
	os.WriteFile(filepath.Join(cfgDir, "settings.json"), sb, 0o644) //nolint:errcheck
	t.Setenv("SCODE_DIR", cfgDir)
	t.Chdir(proj)

	app, err := cli.Setup(cli.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck

	m := newModel(app, make(chan any, 4))
	if !m.input.Focused() {
		t.Fatal("input not focused after newModel")
	}
	if len(m.blocks) == 0 {
		t.Fatal("welcome blocks missing")
	}
}

func chatSSE(w http.ResponseWriter, content string) {
	w.Header().Set("content-type", "text/event-stream")
	chunks := []string{`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`}
	// Split into multi-rune deltas: real providers stream many small
	// deltas per reply — a single-blob mock once masked a value-copy
	// bug that only the second delta triggered.
	rs := []rune(content)
	const step = 4
	for i := 0; i < len(rs); i += step {
		end := i + step
		if end > len(rs) {
			end = len(rs)
		}
		cb, _ := json.Marshal(string(rs[i:end]))
		chunks = append(chunks, `{"choices":[{"index":0,"delta":{"content":`+string(cb)+`}}]}`)
	}
	chunks = append(chunks,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		`[DONE]`,
	)
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
}

// Headless replay of the real flow: submit a prompt, pump every UI
// message through Update until the run finishes. A panic anywhere in
// the update/render path surfaces here.
func TestSubmitRunFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatSSE(w, "hello from model")
	}))
	defer srv.Close()
	cfgDir := t.TempDir()
	proj := t.TempDir()
	settings := map[string]any{
		"defaultProvider": "openai-compat",
		"providers": map[string]any{
			"openai-compat": map[string]any{"apiKey": "k", "baseUrl": srv.URL, "model": "m"},
		},
	}
	sb, _ := json.Marshal(settings)
	os.WriteFile(filepath.Join(cfgDir, "settings.json"), sb, 0o644) //nolint:errcheck
	t.Setenv("SCODE_DIR", cfgDir)
	t.Chdir(proj)

	app, err := cli.Setup(cli.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck

	ui := make(chan any, 256)
	m := newModel(app, ui)
	// Simulate a terminal resize and typing "hi" + Enter.
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(model)
	for _, r := range "hi" {
		tm, _ = m.handleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = tm.(model)
	}
	tm, _ = m.handleKey(tea.KeyPressMsg{Code: '\r'})
	m = tm.(model)
	if !m.running {
		t.Fatal("run did not start on enter")
	}
	timeout := time.After(10 * time.Second)
	for {
		select {
		case msg := <-ui:
			tm, _ := m.Update(msg)
			m = tm.(model)
			// Render every state like the program loop would.
			_ = m.View()
			if _, ok := msg.(runDoneMsg); ok {
				goto done
			}
		case <-timeout:
			t.Fatal("timeout waiting for run completion")
		}
	}
done:
	var content string
	for _, b := range m.blocks {
		content += stripANSI(b.rendered) + "\n"
	}
	if !strings.Contains(content, "hello from model") {
		t.Fatalf("reply missing from transcript:\n%s", content)
	}
	if m.running {
		t.Fatal("still running after runDoneMsg")
	}
}

// setupTestApp builds a cli.App against a mock provider server.
func setupTestApp(t *testing.T, handler http.HandlerFunc) *cli.App {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfgDir := t.TempDir()
	proj := t.TempDir()
	settings := map[string]any{
		"defaultProvider": "openai-compat",
		"providers": map[string]any{
			"openai-compat": map[string]any{"apiKey": "k", "baseUrl": srv.URL, "model": "m"},
		},
	}
	sb, _ := json.Marshal(settings)
	os.WriteFile(filepath.Join(cfgDir, "settings.json"), sb, 0o644) //nolint:errcheck
	t.Setenv("SCODE_DIR", cfgDir)
	t.Chdir(proj)
	app, err := cli.Setup(cli.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() }) //nolint:errcheck
	return app
}

// Sweep window sizes (including degenerate ones a real terminal can
// report) while streaming and while an approval is pending: no panic.
func TestDegenerateWindowSizes(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked: %v\n%s", r, debug.Stack())
		}
	}()
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	for _, size := range [][2]int{{0, 0}, {1, 1}, {5, 2}, {10, 3}, {40, 10}, {80, 24}, {200, 60}} {
		m := newModel(app, make(chan any, 16))
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventThinkingDelta, Delta: "思考中……"}})
		m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextDelta, Delta: strings.Repeat("回复内容 line. ", 30)}})
		// Hostile payloads: invalid UTF-8 at a chunk boundary, emoji,
		// markdown fences, bare \r, control chars.
		m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextDelta, Delta: string([]byte{0xff, 0xfe, 'a'})}})
		m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextDelta, Delta: "```go\nfmt.Println(\"x\")\n``` 🚀 \r\n done\t\u0001"}})
		m.syncViewport()
		m.View()
		m.pending = &approvalRequest{kind: "tool", title: "approval required (rule \"bash\")", body: "  bash command: rm -rf /tmp/x", hint: "[y] [n] [a] [p]", answer: make(chan string, 1)}
		m.resize()
		m.View()
		m.pending = nil
		m.resize()
		m.View()
	}
}

// Scrolling: pgup/pgdown and the mouse wheel move the transcript
// viewport (regression: PageDown's key name is "pgdown", and wheel
// events need routing plus MouseMode on the view).
func TestTranscriptScrolling(t *testing.T) {
	app := setupTestApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	m := newModel(app, make(chan any, 16))
	for i := range 50 {
		m.blocks = append(m.blocks, block{rendered: fmt.Sprintf("line %d", i)})
	}
	m.vp.SetHeight(10)
	m.rebuildRendered()
	m.syncViewport()
	if !m.vp.AtBottom() {
		t.Fatal("expected bottom after rebuild")
	}

	// pgup scrolls up.
	tm, _ := m.handleKey(tea.KeyPressMsg{Code: tea.KeyPgUp})
	m = tm.(model)
	if m.vp.AtBottom() {
		t.Fatal("pgup did not scroll up")
	}
	// pgdown scrolls back down (key name is "pgdown" in v2).
	tm, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
	m = tm.(model)
	if !m.vp.AtBottom() {
		t.Fatal("pgdown did not scroll down")
	}
	// Mouse wheel: routed to the transcript viewport.
	tm, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	m = tm.(model)
	if m.vp.AtBottom() {
		t.Fatal("wheel up did not scroll")
	}
	// The view enables mouse reporting (without it the terminal never
	// sends wheel events at all).
	if v := m.View(); v.MouseMode != tea.MouseModeCellMotion {
		t.Fatalf("MouseMode = %v, want cell motion", v.MouseMode)
	}
}

func TestRenderEventStreamsText(t *testing.T) {
	m := newTestModel()
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextDelta, Delta: "hello"}})
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextDelta, Delta: " world"}})
	if got := m.live.text(); got != "hello world" {
		t.Fatalf("live = %q", got)
	}
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextEnd}})
	if m.live != nil {
		t.Fatalf("live not flushed: %q", m.live.text())
	}
	if len(m.blocks) != 1 { // the body only — spacing leads items, nothing trails
		t.Fatalf("blocks = %#v", m.blocks)
	}
	// The body block carries the white content dot and aligns its text
	// at the gutter column.
	if got := plainText(m.blocks[0].rendered); got != " ● hello world" {
		t.Fatalf("body block = %q", got)
	}
}

// plainText strips ANSI styling from s (test helper).
func plainText(s string) string {
	return sgrRe.ReplaceAllString(s, "")
}

func TestRenderEventToolFailure(t *testing.T) {
	m := newTestModel()
	call := llm.Block{Kind: llm.BlockToolCall, ID: "c1", Name: "bash", Arguments: []byte(`{"command":"ls"}`)}
	m.renderEvent(agent.Event{Type: agent.EvToolStart, Call: &call})
	// The tool row shows the capitalized name under the running (gray) dot.
	if len(m.blocks) != 1 || !strings.Contains(plainText(m.blocks[0].rendered), "Bash") {
		t.Fatalf("tool block = %#v", m.blocks)
	}
	res := agent.ToolResult{IsError: true}
	m.renderEvent(agent.Event{Type: agent.EvToolEnd, Call: &call, Result: &res})
	// The SAME block settles in place (no duplicate row) with the red
	// dot, plus one dimmed error-excerpt line attached below it — no
	// trailing spacer (spacing leads items).
	if len(m.blocks) != 2 {
		t.Fatalf("blocks = %#v", m.blocks)
	}
	if !strings.Contains(m.blocks[0].rendered, "\x1b[31m") {
		t.Fatalf("settled tool block not red-dotted: %q", m.blocks[0].rendered)
	}
	if got := plainText(m.blocks[1].rendered); !strings.Contains(got, "✗") || !strings.Contains(got, "Bash 失败") {
		t.Fatalf("failure excerpt = %q", got)
	}
}

// Approvals round-trip: the blocked Ask unblocks when the model
// answers the pending request via a keystroke.
func TestApprovalRoundTrip(t *testing.T) {
	ui := make(chan any, 4)
	a := newApprover(ui)
	call := llm.Block{Kind: llm.BlockToolCall, Name: "bash", Arguments: []byte(`{"command":"rm -rf /tmp/x"}`)}
	rule := &permission.Rule{Raw: "bash"}

	resCh := make(chan struct {
		allow bool
		rule  string
	}, 1)
	go func() {
		r := a.Ask(context.Background(), call, rule, "bash(rm -rf /tmp/x)")
		resCh <- struct {
			allow bool
			rule  string
		}{r.Allow, r.SessionRule}
	}()

	m := newTestModel()
	select {
	case msg := <-ui:
		am, ok := msg.(approvalMsg)
		if !ok {
			t.Fatalf("ui msg = %T, want approvalMsg", msg)
		}
		m.pending = am.req
	case <-time.After(2 * time.Second):
		t.Fatal("no approval request reached the UI stream")
	}
	if m.pending.title == "" || !strings.Contains(m.pending.hint, "[a]") {
		t.Fatalf("request = %+v", m.pending)
	}
	if !m.answerKey("a") {
		t.Fatal("answerKey(a) rejected")
	}
	select {
	case r := <-resCh:
		if !r.allow || r.rule != "bash(rm -rf /tmp/x)" {
			t.Fatalf("result = %+v, want allow with session rule", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ask did not unblock")
	}
}

// A run cancellation resolves a pending ask with a deny reason.
func TestApprovalInterrupted(t *testing.T) {
	ui := make(chan any, 4)
	a := newApprover(ui)
	ctx, cancel := context.WithCancel(context.Background())
	resCh := make(chan string, 1)
	go func() {
		r := a.Ask(ctx, llm.Block{Kind: llm.BlockToolCall, Name: "bash"}, &permission.Rule{Raw: "bash"}, "bash")
		resCh <- r.Reason
	}()
	<-ui // swallow the request; nobody answers
	cancel()
	select {
	case reason := <-resCh:
		if reason == "" {
			t.Fatal("interrupted ask returned no reason")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ask did not unblock on cancel")
	}
}

func TestHumanTokens(t *testing.T) {
	for in, want := range map[int64]string{0: "0", 999: "999", 1000: "1k", 1530: "1.5k", 2_000_000: "2M"} {
		if got := humanTokens(in); got != want {
			t.Errorf("humanTokens(%d) = %q, want %q", in, got, want)
		}
	}
}

// stripANSI drops CSI sequences for content assertions: ESC '['
// parameter bytes, then the one final byte in [@~]. (The old loop
// stopped AT '[' — 0x5b is printable — so "…\x1b[90m●…" left "90m●"
// residue and assertions crossing a style boundary never matched.)
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			b.WriteByte(s[i])
			continue
		}
		j := i + 1
		if j < len(s) && s[j] == '[' {
			j++
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
		}
		if j < len(s) {
			j++ // consume the final byte (SGR's 'm', etc.)
		}
		i = j - 1
	}
	return b.String()
}

// Regression: streamed lines must wrap at the gutter-adjusted width.
// Lines cached at the full terminal width gained 3 gutter cells on the
// committed re-wrap and got cut into a full row plus an orphan 1-3
// cell row at column 0.
func TestStreamingLongLinesRespectGutter(t *testing.T) {
	m := newTestModel()                    // width 80
	long := strings.Repeat("a", 78) + "\n" // > width-gutter(77): must wrap twice
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextDelta, Delta: long}})
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextDelta, Delta: strings.Repeat("b", 30)}})
	m.renderEvent(agent.Event{Type: agent.EvLLM, LLM: &llm.Event{Type: llm.EventTextEnd}})
	if len(m.blocks) < 1 {
		t.Fatal("no body block committed")
	}
	for i, b := range m.blocks {
		for j, line := range b.lines {
			if w := ansi.StringWidth(line); w > m.width {
				t.Fatalf("block %d line %d width %d > %d: %q", i, j, w, m.width, line)
			}
		}
	}
}

// Regression: ctrl+c during a tool/sandbox ask must abort the pending
// run. The ask branch used to swallow it (answerKey ignores the key),
// so the user could neither interrupt nor quit.
func TestCtrlCAbortsDuringToolApproval(t *testing.T) {
	answered := make(chan string, 1)
	m := newTestModel()
	m.pending = &approvalRequest{kind: "tool", title: "工具审批", answer: answered}
	m.running = true
	cancelled := make(chan struct{})
	m.cancel = func() { close(cancelled) }

	tm, _ := m.handleKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m = tm.(model)
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("ctrl+c did not cancel the run")
	}
	if m.pending != nil {
		t.Fatal("ctrl+c left the approval prompt pending")
	}
	select {
	case got := <-answered:
		if got != "" {
			t.Fatalf("ask answer = %q, want interruption", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the ask was never unblocked")
	}
}

// Double-ctrl+c quit guard: a single ctrl+c only arms the guard with a
// hint toast; the session quits on the second press inside the window,
// any other key or the expiry tick disarms it.
func TestCtrlCTwiceQuits(t *testing.T) {
	ctrlC := tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	m := newTestModel()

	// First press: armed, no quit command, hint toast visible.
	tm, cmd := m.handleKey(ctrlC)
	m = tm.(model)
	if cmd == nil {
		t.Fatal("first ctrl+c should schedule the disarm tick")
	}
	if !m.quitArmed {
		t.Fatal("first ctrl+c did not arm the quit guard")
	}
	if m.toast == nil || !strings.Contains(m.toast.text, "ctrl+c") {
		t.Fatalf("confirm hint toast missing: %+v", m.toast)
	}

	// Another key disarms.
	tm, _ = m.handleKey(tea.KeyPressMsg{Code: 'x', Text: "x"})
	m = tm.(model)
	if m.quitArmed {
		t.Fatal("typing did not disarm the quit guard")
	}

	// Re-arm, then the expiry tick (matching id) disarms; a stale tick
	// must not.
	tm, _ = m.handleKey(ctrlC)
	m = tm.(model)
	tm, _ = m.Update(quitDisarmMsg{id: m.quitSeq + 100})
	m = tm.(model)
	if !m.quitArmed {
		t.Fatal("stale disarm tick cleared the guard")
	}
	tm, _ = m.Update(quitDisarmMsg{id: m.quitSeq})
	m = tm.(model)
	if m.quitArmed {
		t.Fatal("expiry tick did not disarm the guard")
	}

	// Two presses in a row quit (bubbletea signals via the Quit cmd;
	// tea.Quit is a nil-receiver func value, so compare against a
	// quitter sentinel through the message it produces).
	tm, _ = m.handleKey(ctrlC)
	m = tm.(model)
	tm, quitCmd := m.handleKey(ctrlC)
	if quitCmd == nil {
		t.Fatal("second ctrl+c produced no command")
	}
	if _, ok := quitCmd().(tea.QuitMsg); !ok {
		t.Fatal("second ctrl+c did not quit")
	}
}

// Regression: tool rows are separated by blank lines — including
// PARALLEL batches, whose rows land consecutively at start time (the
// old model's trailing spacer piled up at the batch's end instead of
// sitting between the rows).
func TestParallelToolRowsSpaced(t *testing.T) {
	m := newTestModel()
	a := llm.Block{Kind: llm.BlockToolCall, ID: "c1", Name: "read", Arguments: []byte(`{"path":"a.go"}`)}
	b := llm.Block{Kind: llm.BlockToolCall, ID: "c2", Name: "read", Arguments: []byte(`{"path":"b.go"}`)}
	m.renderEvent(agent.Event{Type: agent.EvToolStart, Call: &a})
	m.renderEvent(agent.Event{Type: agent.EvToolStart, Call: &b})
	// [rowA, spacer, rowB] on a fresh transcript (the first item never
	// leads with a gap); in place settles keep the spacer between them.
	if len(m.blocks) != 3 || !m.blocks[1].spacer ||
		m.blocks[0].spacer || m.blocks[2].spacer {
		t.Fatalf("blocks = %#v", m.blocks)
	}
	if got := plainText(m.vp.GetContent()); !strings.Contains(got, "\n\n") {
		t.Fatalf("no blank line between tool rows:\n%s", got)
	}
	m.renderEvent(agent.Event{Type: agent.EvToolEnd, Call: &a, Result: &agent.ToolResult{}})
	m.renderEvent(agent.Event{Type: agent.EvToolEnd, Call: &b, Result: &agent.ToolResult{}})
	if len(m.blocks) != 3 || !m.blocks[1].spacer {
		t.Fatalf("settles changed the spacing: %#v", m.blocks)
	}
}

// The hardware cursor anchors to the composer's text end (IME
// candidate windows follow it — a stranded cursor lands the candidate
// box on the status bar); a keyboard-owning modal drops it.
func TestViewCursorAnchor(t *testing.T) {
	app := setupPaletteApp(t)
	m := newModel(app, make(chan any, 16))
	if m.input.VirtualCursor() {
		t.Fatal("virtual cursor must be off for the real cursor to track")
	}
	v := m.view()
	if v.Cursor == nil {
		t.Fatal("no cursor anchored at the composer")
	}
	x0, y0 := v.Cursor.Position.X, v.Cursor.Position.Y

	// Typing a full-width rune moves the anchor two cells right.
	m.input.InsertString("你")
	v = m.view()
	if v.Cursor.Position.X != x0+2 || v.Cursor.Position.Y != y0 {
		t.Fatalf("cursor did not track the text end: (%d,%d) -> (%d,%d)",
			x0, y0, v.Cursor.Position.X, v.Cursor.Position.Y)
	}
	// The anchor sits inside the input box: one top-rule line below the
	// viewport part.
	if want := lipgloss.Height(m.renderViewport()) + 1; v.Cursor.Position.Y != want {
		t.Fatalf("cursor Y = %d, want %d (viewport + top rule)", v.Cursor.Position.Y, want)
	}

	// A modal that hides the input drops the cursor (renderer hides it).
	m.cfgOpen = true
	if v = m.view(); v.Cursor != nil {
		t.Fatal("modal should drop the composer cursor")
	}
}
