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
	"sync/atomic"
	"testing"

	"scode/internal/memory"
)

// memorySrv branches: the extraction call (its system prompt carries
// "long-term memory") answers with JSON ops; ordinary turns stream
// text.
func memorySrv(t *testing.T, calls *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "long-term memory") {
			w.Header().Set("content-type", "text/event-stream")
			ops := `[{"op":"add","category":"project","content":"构建: go build ./..."},{"op":"add","category":"preference","content":"偏好: 回复用中文"}]`
			cb, _ := json.Marshal(ops)
			chunks := []string{
				`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
				`{"choices":[{"index":0,"delta":{"content":` + string(cb) + `}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":10}}`,
				`[DONE]`,
			}
			for _, c := range chunks {
				w.Write([]byte("data: " + c + "\n\n")) //nolint:errcheck
			}
			return
		}
		if calls != nil {
			atomic.AddInt32(calls, 1)
		}
		chatSSE(w, "conversation reply", 10)
	}))
}

// The manual trigger: extraction runs a side-channel call, parses the
// ops, and the store lands on disk.
func TestMemoryExtractManual(t *testing.T) {
	srv := memorySrv(t, nil)
	defer srv.Close()
	setupTestApp(t, srv)
	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	// A conversation must exist for a digest.
	if err := runPrompt(context.Background(), app, &Renderer{Out: io.Discard, Err: io.Discard}, "记住我们用 go build"); err != nil {
		t.Fatal(err)
	}

	summary, err := app.MemoryExtract(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "新增 2") {
		t.Fatalf("summary = %q", summary)
	}
	entries, _ := app.memStore.Load()
	if len(entries) != 2 || entries[0].Category == "" {
		t.Fatalf("store = %+v", entries)
	}

	// The listing shows the categories.
	if out := app.MemoryListText(); !strings.Contains(out, "构建: go build ./...") || !strings.Contains(out, "[项目]") {
		t.Fatalf("listing:\n%s", out)
	}
	app.Close() //nolint:errcheck
}

// Injection: autoMemory on injects the memory section; relevance on
// filters to hand-picked entries; autoMemory off injects nothing.
func TestMemoryInjection(t *testing.T) {
	srv := memorySrv(t, nil)
	defer srv.Close()
	setupTestApp(t, srv)

	memPath := memory.StorePath(os.Getenv("SCODE_DIR"), mustCWD(t))
	os.MkdirAll(filepath.Dir(memPath), 0o755) //nolint:errcheck
	writeJSON(t, memPath, map[string]any{"memories": []map[string]any{
		{"id": "m1", "category": "project", "content": "构建用 build.bat"},
		{"id": "m2", "category": "preference", "content": "回复用中文", "selected": true},
	}})

	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasSection(app, "memory", "构建用 build.bat") || !hasSection(app, "memory", "回复用中文") {
		t.Fatal("memory section missing with autoMemory on")
	}
	app.Close() //nolint:errcheck

	// Relevance on: only the hand-picked entry rides.
	writeSetting(t, "memoryRelevance", true)
	app, err = Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if hasSection(app, "memory", "构建用 build.bat") {
		t.Fatal("unselected memory leaked through the relevance filter")
	}
	if !hasSection(app, "memory", "回复用中文") {
		t.Fatal("selected memory missing")
	}
	app.Close() //nolint:errcheck

	// autoMemory off: no section at all.
	writeSetting(t, "autoMemory", false)
	app, err = Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if hasSection(app, "memory", "") {
		t.Fatal("memory section present with autoMemory off")
	}
	// And extraction refuses.
	if _, err := app.MemoryExtract(context.Background()); err == nil {
		t.Fatal("extraction should refuse with autoMemory off")
	}
	app.Close() //nolint:errcheck
}

// Close-time auto-extraction: on for interactive sessions with the
// switch on; off otherwise (print runs never write memories).
func TestMemoryAutoExtractOnClose(t *testing.T) {
	var conv int32
	srv := memorySrv(t, &conv)
	defer srv.Close()
	setupTestApp(t, srv)

	// Switch off by default: a conversation + interactive close writes
	// nothing.
	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	app.interactive = true
	runPrompt(context.Background(), app, &Renderer{Out: io.Discard, Err: io.Discard}, "hi there") //nolint:errcheck
	app.Close()                                                                                   //nolint:errcheck
	if entries, _ := memory.Open(os.Getenv("SCODE_DIR"), mustCWD(t)).Load(); len(entries) != 0 {
		t.Fatalf("auto-extraction ran with the switch off: %+v", entries)
	}

	// On: the close path runs the side-channel extraction.
	writeSetting(t, "memoryAutoExtraction", true)
	app2, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	app2.interactive = true
	runPrompt(context.Background(), app2, &Renderer{Out: io.Discard, Err: io.Discard}, "another turn") //nolint:errcheck
	app2.Close()                                                                                       //nolint:errcheck
	entries, _ := memory.Open(os.Getenv("SCODE_DIR"), mustCWD(t)).Load()
	if len(entries) != 2 {
		t.Fatalf("auto-extraction results = %+v", entries)
	}

	// A non-interactive (print-mode) session never extracts.
	os.Remove(memory.StorePath(os.Getenv("SCODE_DIR"), mustCWD(t))) //nolint:errcheck
	app3, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	runPrompt(context.Background(), app3, &Renderer{Out: io.Discard, Err: io.Discard}, "print mode") //nolint:errcheck
	app3.Close()                                                                                     //nolint:errcheck
	if entries, _ = memory.Open(os.Getenv("SCODE_DIR"), mustCWD(t)).Load(); len(entries) != 0 {
		t.Fatalf("print-mode run wrote memories: %+v", entries)
	}
}

func hasSection(app *App, name, contains string) bool {
	for _, s := range app.Tr.Messages()[0].Sections {
		if s.Name == name && (contains == "" || strings.Contains(s.Value, contains)) {
			return true
		}
	}
	return false
}

func mustCWD(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func writeJSON(t *testing.T, path string, doc map[string]any) {
	t.Helper()
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, out, 0o644) //nolint:errcheck
}
