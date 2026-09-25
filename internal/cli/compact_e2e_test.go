package cli

import (
	"context"
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

	"scode/internal/agent"
	"scode/internal/llm"
)

// chatSSE writes one chat.completions streaming response.
func chatSSE(w http.ResponseWriter, content string, promptTokens int64) {
	w.Header().Set("content-type", "text/event-stream")
	chunks := []string{
		fmt.Sprintf(`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`),
		fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%s}}]}`, mustMarshalString(content)),
		fmt.Sprintf(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":0}}}`, promptTokens),
		`[DONE]`,
	}
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
}

func mustMarshalString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// End-to-end compaction wiring: a run whose usage exceeds the threshold
// triggers the summarization call (CacheNone side), appends the marker
// to the session file, and rebuilds the in-memory transcript from the
// projection.
func TestCompactionWiringE2E(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := ioReadAll(r)
		var req struct {
			Tools    []any  `json:"tools"`
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		isSummary := strings.Contains(fmt.Sprint(req.Messages), "summarize coding-agent")
		if atomic.AddInt32(&calls, 1) == 1 {
			chatSSE(w, "done", 50_000) // large usage triggers compaction
			return
		}
		if !isSummary {
			chatSSE(w, "unexpected", 10)
			return
		}
		chatSSE(w, "SUMMARY: task completed, files verified.", 10)
	}))
	defer srv.Close()

	cfgDir := t.TempDir()
	proj := t.TempDir()
	settings := map[string]any{
		"defaultProvider":  "openai-compat",
		"compactionTokens": 1000,
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

	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	out := make(chan agent.Event, 256)
	if err := app.Run(context.Background(), out, "do the thing"); err != nil {
		t.Fatal(err)
	}
	for range out { // drain remaining
	}

	// Projection rebuilt: leading system + summary user message.
	msgs := app.Tr.Messages()
	if len(msgs) != 2 {
		t.Fatalf("projected transcript len = %d, want 2: %+v", len(msgs), msgs)
	}
	if msgs[1].Role != llm.RoleUser || !strings.Contains(msgs[1].Content[0].Text, "SUMMARY: task completed") {
		t.Fatalf("summary message = %+v", msgs[1])
	}

	// Storage keeps the full history plus the marker.
	data, err := os.ReadFile(app.Sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	file := string(data)
	if !strings.Contains(file, `"kind":"compaction"`) {
		t.Fatal("compaction marker missing from session file")
	}
	if !strings.Contains(file, "do the thing") {
		t.Fatal("original user message missing — history must be preserved")
	}
}

func ioReadAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}
