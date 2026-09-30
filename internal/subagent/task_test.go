package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"scode/internal/agent"
	"scode/internal/config"
	"scode/internal/llm"
)

// subTestHost builds a Host whose Resolve serves a fresh openai-compat
// provider over srv; spend and progress are recorded for assertions.
func subTestHost(t *testing.T, srv *httptest.Server, specs []Spec) (Host, *[]string, *[]llm.Usage) {
	t.Helper()
	settings := &config.Settings{
		DefaultProvider: "testprov",
		Providers: map[string]config.ProviderConfig{
			"testprov": {APIKey: "k", BaseURL: srv.URL, Model: "m"},
		},
	}
	var mu sync.Mutex
	var progress []string
	var spend []llm.Usage
	return Host{
		Specs: func() ([]Spec, error) { return specs, nil },
		Resolve: func(model string) (llm.Provider, llm.Model, string, error) {
			p, id, name, err := config.ResolveProvider(settings, "testprov", "m")
			if err != nil {
				return nil, llm.Model{}, "", err
			}
			return p, buildTestModel(settings, name, id), id, nil
		},
		Env: func() map[string]string { return map[string]string{} },
		CWD: func() string { return t.TempDir() },
		Sandbox: func() *agent.SandboxPolicy {
			return &agent.SandboxPolicy{Mode: "read-only"}
		},
		Spend: func(u llm.Usage) {
			mu.Lock()
			spend = append(spend, u)
			mu.Unlock()
		},
	}, &progress, &spend
}

// buildTestModel mirrors cli's buildModel enough for the loop (provider
// + id + a sane output cap).
func buildTestModel(s *config.Settings, providerName, modelID string) llm.Model {
	m := llm.Model{ID: modelID, Provider: providerName, MaxTokens: 4096}
	m.Reasoning = s.Providers[providerName].Reasoning
	return m
}

func subSSE(w http.ResponseWriter, content string, usage bool) {
	w.Header().Set("content-type", "text/event-stream")
	chunks := []string{`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`}
	cb, _ := json.Marshal(content)
	chunks = append(chunks, `{"choices":[{"index":0,"delta":{"content":`+string(cb)+`}}]}`)
	u := ""
	if usage {
		u = `,"usage":{"prompt_tokens":100,"completion_tokens":20}`
	}
	chunks = append(chunks,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`+u+`}`,
		`[DONE]`)
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
}

// The engine: an unknown agent errors with the configured names; a
// known one runs an ISOLATED loop (own context, read-only tools) and
// returns the report with the footer, folding usage and progress.
func TestTaskToolRun(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := ioReadAll(r)
		// The sub-agent's request must carry ONLY the read-only tools —
		// isolation is structural, not advisory.
		if strings.Contains(string(body), `"edit"`) || strings.Contains(string(body), `"write"`) || strings.Contains(string(body), `"bash"`) {
			t.Errorf("sub-agent request carries a mutating tool:\n%s", string(body))
		}
		subSSE(w, "the answer is in internal/agent/loop.go:233", true)
	}))
	defer srv.Close()

	specs := []Spec{{Name: "researcher", Description: "代码考古", Model: ""}}
	host, _, spend := subTestHost(t, srv, specs)
	tool := NewTaskTool(host)

	// Unknown agent → error listing the configured names.
	res := tool.Execute(agent.ToolContext{Ctx: context.Background(), CWD: t.TempDir()},
		json.RawMessage(`{"agent":"nope","prompt":"x"}`))
	if !res.IsError || !strings.Contains(resultText(res), `researcher`) {
		t.Fatalf("unknown-agent result = %+v", res)
	}

	// The real run: progress lines stream, usage folds, the report
	// carries the footer.
	var progress []string
	tc := agent.ToolContext{
		Ctx:      context.Background(),
		CWD:      t.TempDir(),
		Progress: func(line string) { progress = append(progress, line) },
	}
	res = tool.Execute(tc, json.RawMessage(`{"agent":"researcher","prompt":"where is emit defined?"}`))
	if res.IsError {
		t.Fatalf("run failed: %+v", res)
	}
	text := resultText(res)
	if !strings.Contains(text, "the answer is in internal/agent/loop.go:233") {
		t.Fatalf("report missing:\n%s", text)
	}
	if !strings.Contains(text, "(sub-agent researcher · 1 turns") {
		t.Fatalf("footer missing:\n%s", text)
	}
	if len(*spend) == 0 || (*spend)[0].Input != 100 {
		t.Fatalf("usage not folded: %+v", *spend)
	}
	if len(progress) == 0 || !strings.Contains(progress[0], "researcher") {
		t.Fatalf("progress not streamed: %v", progress)
	}
	if calls != 1 {
		t.Fatalf("provider calls = %d, want 1", calls)
	}
}

// The shipped built-in works with NO agents.json at all — delegation
// is useful out of the box.
func TestTaskToolBuiltinAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subSSE(w, "researched", false)
	}))
	defer srv.Close()

	host, _, _ := subTestHost(t, srv, nil) // no user specs
	tool := NewTaskTool(host)
	res := tool.Execute(agent.ToolContext{Ctx: context.Background(), CWD: t.TempDir()},
		json.RawMessage(`{"agent":"general-purpose","prompt":"look around"}`))
	if res.IsError {
		t.Fatalf("built-in delegate failed: %+v", res)
	}
	if text := resultText(res); !strings.Contains(text, "researched") || !strings.Contains(text, "(sub-agent general-purpose ·") {
		t.Fatalf("built-in report = %q", text)
	}
}

// A multi-turn delegate: it runs a real read tool first, then reports
// — no turn cap exists; the run ends when the delegate finishes (or
// the parent aborts).
func TestTaskToolMultiTurn(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x.txt"), []byte("the answer is 42\n"), 0o644) //nolint:errcheck

	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		w.Header().Set("content-type", "text/event-stream")
		var chunks []string
		if call == 1 { // turn 1: read the file
			chunks = []string{
				`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"read","arguments":"{\"path\":\"x.txt\"}"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`[DONE]`,
			}
		} else { // turn 2: report
			cb, _ := json.Marshal("x.txt says: the answer is 42")
			chunks = []string{
				`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
				`{"choices":[{"index":0,"delta":{"content":` + string(cb) + `}}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":7}}`,
				`[DONE]`,
			}
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
	}))
	defer srv.Close()

	specs := []Spec{{Name: "wanderer"}}
	host, _, _ := subTestHost(t, srv, specs)
	host.CWD = func() string { return dir }
	tool := NewTaskTool(host)

	var progress []string
	tc := agent.ToolContext{
		Ctx:      context.Background(),
		CWD:      dir,
		Progress: func(line string) { progress = append(progress, line) },
	}
	res := tool.Execute(tc, json.RawMessage(`{"agent":"wanderer","prompt":"read x.txt and report"}`))
	if res.IsError {
		t.Fatalf("multi-turn run failed: %+v", res)
	}
	text := resultText(res)
	if !strings.Contains(text, "the answer is 42") {
		t.Fatalf("report missing:\n%s", text)
	}
	if !strings.Contains(text, "(sub-agent wanderer · 2 turns · 1 tool calls") {
		t.Fatalf("footer counts wrong:\n%s", text)
	}
	if len(progress) == 0 || !strings.Contains(progress[len(progress)-1], "read") {
		t.Fatalf("tool step not streamed to progress: %v", progress)
	}
}

func resultText(r agent.ToolResult) string {
	for _, b := range r.Content {
		if b.Text != "" {
			return b.Text
		}
	}
	return ""
}

func ioReadAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	b := make([]byte, 0)
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		b = append(b, buf[:n]...)
		if err != nil {
			break
		}
	}
	return b, nil
}
