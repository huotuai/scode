package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"scode/internal/agent"
)

// chatSSEToolCall writes one streaming response carrying a single
// tool call (chat.completions wire shape).
func chatSSEToolCall(w http.ResponseWriter, name, args string) {
	w.Header().Set("content-type", "text/event-stream")
	chunks := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":%s,"arguments":%s}}]}}]}`,
			mustMarshalString(name), mustMarshalString(args)),
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		`[DONE]`,
	}
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
}

// setupTestAppWithPermissions is setupTestApp plus a permissions section.
func setupTestAppWithPermissions(t *testing.T, srv *httptest.Server, perms map[string]any) {
	t.Helper()
	cfgDir := t.TempDir()
	proj := t.TempDir()
	settings := map[string]any{
		"defaultProvider":  "openai-compat",
		"compactionTokens": -1,
		"permissions":      perms,
		"providers": map[string]any{
			"openai-compat": map[string]any{
				"apiKey":  "test",
				"baseUrl": srv.URL,
				"model":   "test-model",
			},
		},
	}
	sb, _ := json.Marshal(settings)
	os.WriteFile(filepath.Join(cfgDir, "settings.json"), sb, 0o644) //nolint:errcheck
	t.Setenv("SCODE_DIR", cfgDir)
	t.Chdir(proj)
}

// End-to-end: a deny rule turns the tool call into an error result the
// model reacts to — the bash tool never executes.
func TestPermissionDenyE2E(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			chatSSEToolCall(w, "bash", `{"command":"rm -rf important"}`)
			return
		}
		chatSSE(w, "understood, staying away from rm", 10)
	}))
	defer srv.Close()
	setupTestAppWithPermissions(t, srv, map[string]any{
		"deny": []string{"bash(rm:*)"},
	})

	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck

	out := make(chan agent.Event, 256)
	var toolErr string
	done := make(chan error, 1)
	go func() { done <- app.Run(context.Background(), out, "clean up") }()
	for ev := range out {
		if ev.Type == agent.EvToolEnd && ev.Result != nil && ev.Result.IsError {
			toolErr = ev.Result.Content[0].Text
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(toolErr, `permission denied by rule "bash(rm:*)"`) {
		t.Fatalf("tool result should carry the deny reason, got %q", toolErr)
	}
}

// End-to-end: an ask rule under a non-interactive approver (print mode
// shape) denies with guidance instead of executing.
func TestPermissionAskNonInteractiveE2E(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			chatSSEToolCall(w, "bash", `{"command":"terraform apply"}`)
			return
		}
		chatSSE(w, "ok, skipping apply", 10)
	}))
	defer srv.Close()
	setupTestAppWithPermissions(t, srv, map[string]any{
		"ask": []string{"bash(terraform:*)"},
	})

	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close() //nolint:errcheck

	out := make(chan agent.Event, 256)
	var toolErr string
	done := make(chan error, 1)
	go func() { done <- app.Run(context.Background(), out, "apply infra") }()
	for ev := range out {
		if ev.Type == agent.EvToolEnd && ev.Result != nil && ev.Result.IsError {
			toolErr = ev.Result.Content[0].Text
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(toolErr, "non-interactive") {
		t.Fatalf("tool result should explain the non-interactive deny, got %q", toolErr)
	}
}
