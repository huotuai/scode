//go:build windows || linux || darwin

package tui

// End-to-end test driving the REAL bubbletea program loop with a
// scripted input pipe and a captured output buffer: type "hi", Enter,
// wait for the mock model reply, then Ctrl+C. Reproduces crashes that
// only happen with the live renderer attached.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"scode/internal/cli"
)

// chatSSEBlob writes the reply as ONE delta: the frame-diff renderer
// writes incremental cells, so a mid-stream fragment like "hello fr"
// and "om model" land in separate frames — full-string assertions on
// the raw output stream only work with single-delta replies. (The
// multi-delta path is covered precisely at the model level.)
func chatSSEBlob(w http.ResponseWriter, content string) {
	w.Header().Set("content-type", "text/event-stream")
	cb, _ := json.Marshal(content)
	chunks := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":` + string(cb) + `}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		`[DONE]`,
	}
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
}

// quitProgram sends ctrl+c until the program exits: if a run is still
// in flight the first one only aborts the run (REPL semantics), so a
// second keystroke is needed to quit.
func quitProgram(t *testing.T, pw io.Writer, runDone chan error) {
	t.Helper()
	for range 3 {
		if err := (scriptWriter{pw}).write("\x03"); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-runDone:
			if err != nil {
				t.Fatalf("program error: %v", err)
			}
			return
		case <-time.After(3 * time.Second):
		}
	}
	t.Fatal("program did not exit on ctrl+c")
}

func chatSSEToolCall(w http.ResponseWriter, name, args string) {
	w.Header().Set("content-type", "text/event-stream")
	nb, _ := json.Marshal(name)
	ab, _ := json.Marshal(args)
	chunks := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":` + string(nb) + `,"arguments":` + string(ab) + `}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		`[DONE]`,
	}
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
}

// Tool-call flow through the REAL program: the model calls a tool,
// the permission engine asks, the user presses y, the tool runs, the
// model replies with a long CJK text. Exercises the approval overlay
// and non-ASCII rendering in the live renderer.
func TestProgramToolApproval(t *testing.T) {
	longCJK := strings.Repeat("你好世界，这是一段较长的中文回复内容。", 20)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			chatSSEToolCall(w, "read", `{"path":"note.txt"}`)
			return
		}
		chatSSEBlob(w, longCJK)
	}))
	defer srv.Close()
	cfgDir := t.TempDir()
	proj := t.TempDir()
	settings := map[string]any{
		"defaultProvider": "openai-compat",
		"sandbox":         map[string]any{"mode": "danger-full-access"},
		"permissions":     map[string]any{"ask": []string{"read"}},
		"providers": map[string]any{
			"openai-compat": map[string]any{"apiKey": "k", "baseUrl": srv.URL, "model": "m"},
		},
	}
	sb, _ := json.Marshal(settings)
	os.WriteFile(filepath.Join(cfgDir, "settings.json"), sb, 0o644)           //nolint:errcheck
	os.WriteFile(filepath.Join(proj, "note.txt"), []byte("file body"), 0o644) //nolint:errcheck
	t.Setenv("SCODE_DIR", cfgDir)
	t.Chdir(proj)

	ui := make(chan any, 256)
	app, err := cli.Setup(cli.Options{Approver: newApprover(ui)})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck

	pr, pw := io.Pipe()
	out := &syncWriter{}
	p := tea.NewProgram(newModel(app, ui),
		tea.WithInput(pr),
		tea.WithOutput(out),
		tea.WithWindowSize(100, 30),
		tea.WithoutSignals(),
	)
	runDone := make(chan error, 1)
	go func() { _, err := p.Run(); runDone <- err }()

	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !strings.Contains(stripANSI(out.String()), want) {
			if time.Now().After(deadline) {
				t.Fatalf("%q never rendered; output:\n%s", want, stripANSI(out.String()))
			}
			select {
			case err := <-runDone:
				t.Fatalf("program exited early waiting for %q: %v; output:\n%s", want, err, stripANSI(out.String()))
			default:
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	if err := (scriptWriter{pw}).write("hi\r"); err != nil {
		t.Fatal(err)
	}
	waitFor("工具审批")
	if err := (scriptWriter{pw}).write("y"); err != nil {
		t.Fatal(err)
	}
	waitFor("你好世界")
	time.Sleep(300 * time.Millisecond)
	quitProgram(t, pw, runDone)
}

// chatSSEThinking streams one reasoning_content blob then one content
// blob (GLM/DeepSeek/Kimi thinking-model shape).
func chatSSEThinking(w http.ResponseWriter, thinking, content string) {
	w.Header().Set("content-type", "text/event-stream")
	tb, _ := json.Marshal(thinking)
	cb, _ := json.Marshal(content)
	chunks := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":` + string(tb) + `}}]}`,
		`{"choices":[{"index":0,"delta":{"content":` + string(cb) + `}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		`[DONE]`,
	}
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
}

// Reasoning-stream flow (GLM/DeepSeek shape): reasoning_content deltas
// then content, through the real program.
func TestProgramThinkingStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatSSEThinking(w, "先思考一下这个问题。", "最终答案")
	}))
	defer srv.Close()
	cfgDir := t.TempDir()
	proj := t.TempDir()
	settings := map[string]any{
		"defaultProvider": "openai-compat",
		"providers": map[string]any{
			"openai-compat": map[string]any{"apiKey": "k", "baseUrl": srv.URL, "model": "m", "reasoning": true},
		},
	}
	sb, _ := json.Marshal(settings)
	os.WriteFile(filepath.Join(cfgDir, "settings.json"), sb, 0o644) //nolint:errcheck
	t.Setenv("SCODE_DIR", cfgDir)
	t.Chdir(proj)

	ui := make(chan any, 256)
	app, err := cli.Setup(cli.Options{Approver: newApprover(ui)})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck

	pr, pw := io.Pipe()
	out := &syncWriter{}
	p := tea.NewProgram(newModel(app, ui),
		tea.WithInput(pr),
		tea.WithOutput(out),
		tea.WithWindowSize(100, 30),
		tea.WithoutSignals(),
	)
	runDone := make(chan error, 1)
	go func() { _, err := p.Run(); runDone <- err }()

	if err := (scriptWriter{pw}).write("hi\r"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(stripANSI(out.String()), "最终答案") {
		if time.Now().After(deadline) {
			t.Fatalf("reply never rendered; output:\n%s", stripANSI(out.String()))
		}
		select {
		case err := <-runDone:
			t.Fatalf("program exited early: %v; output:\n%s", err, stripANSI(out.String()))
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	quitProgram(t, pw, runDone)
}

func TestProgramEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatSSEBlob(w, "hello from model")
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

	ui := make(chan any, 256)
	app, err := cli.Setup(cli.Options{Approver: newApprover(ui)})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck

	pr, pw := io.Pipe()
	out := &syncWriter{}
	p := tea.NewProgram(newModel(app, ui),
		tea.WithInput(pr),
		tea.WithOutput(out),
		tea.WithWindowSize(100, 30),
		tea.WithoutSignals(),
	)
	runDone := make(chan error, 1)
	go func() {
		_, err := p.Run()
		runDone <- err
	}()

	// Type "hi" + Enter.
	if err := (scriptWriter{pw}).write("hi\r"); err != nil {
		t.Fatal(err)
	}
	// Wait for the reply to appear in the rendered output.
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(stripANSI(out.String()), "hello from model") {
		if time.Now().After(deadline) {
			t.Fatalf("reply never rendered; output:\n%s", stripANSI(out.String()))
		}
		select {
		case err := <-runDone:
			t.Fatalf("program exited early: %v; output:\n%s", err, stripANSI(out.String()))
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Let the run settle, then quit with Ctrl+C (idle: quits).
	time.Sleep(300 * time.Millisecond)
	quitProgram(t, pw, runDone)
}
