package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCallSSE answers request 1 with a write tool call; request 2
// streams text.
func writeCallSSE(w http.ResponseWriter, path, content string) {
	w.Header().Set("content-type", "text/event-stream")
	args, _ := json.Marshal(map[string]string{"path": path, "content": content})
	ab, _ := json.Marshal(string(args))
	chunks := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"write","arguments":` + string(ab) + `}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		`[DONE]`,
	}
	for _, c := range chunks {
		io.WriteString(w, "data: "+c+"\n\n") //nolint:errcheck
	}
}

// E2E: a real write through the agent's registry snapshots the file's
// before-state; /rewind restores it. The config switch gates capture.
func TestCheckpointE2E(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls++; calls == 1 {
			writeCallSSE(w, "notes.md", "AI overwrote the notes\n")
			return
		}
		chatSSE(w, "done", 10)
	}))
	defer srv.Close()
	setupTestApp(t, srv)

	// A pre-existing file the AI will overwrite.
	proj, _ := os.Getwd() // t.Chdir'd into the temp project
	target := filepath.Join(proj, "notes.md")
	os.WriteFile(target, []byte("the original notes\n"), 0o644) //nolint:errcheck

	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck
	if err := runPrompt(context.Background(), app, &Renderer{Out: io.Discard, Err: io.Discard}, "overwrite notes.md"); err != nil {
		t.Fatal(err)
	}

	// The checkpoint exists and the file was overwritten.
	cps := app.Checkpoints()
	if len(cps) != 1 || cps[0].Tool != "write" {
		t.Fatalf("checkpoints = %+v", cps)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "AI overwrote the notes\n" {
		t.Fatalf("write did not land: %q", data)
	}

	// /rewind restores the original.
	out, _, err := app.Command("/rewind " + cps[0].ID)
	if err != nil || !strings.Contains(out, "恢复 1 个文件") {
		t.Fatalf("rewind out = %q err=%v", out, err)
	}
	data, _ = os.ReadFile(target)
	if string(data) != "the original notes\n" {
		t.Fatalf("restore wrote %q", data)
	}

	// The listing form works too.
	if out, _, _ := app.Command("/rewind"); !strings.Contains(out, "c001") {
		t.Fatalf("listing:\n%s", out)
	}
}

// The /config switch gates capture: off → no snapshots at all.
func TestCheckpointSwitchOff(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls++; calls == 1 {
			writeCallSSE(w, "fresh.md", "created by AI\n")
			return
		}
		chatSSE(w, "done", 10)
	}))
	defer srv.Close()
	setupTestApp(t, srv)
	writeSetting(t, "rewindCheckpoints", false)

	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck
	if err := runPrompt(context.Background(), app, &Renderer{Out: io.Discard, Err: io.Discard}, "create fresh.md"); err != nil {
		t.Fatal(err)
	}
	if cps := app.Checkpoints(); len(cps) != 0 {
		t.Fatalf("captures despite the switch off: %+v", cps)
	}
}
