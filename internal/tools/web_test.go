package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"scode/internal/agent"
)

// webToolContext builds a ToolContext for web tool tests.
func webToolContext() agent.ToolContext {
	return agent.ToolContext{Ctx: context.Background()}
}

// allowPrivateConfig lets web_fetch reach the httptest loopback server.
func allowPrivateConfig() WebConfig {
	return WebConfig{AllowPrivateNetwork: true}
}

func resultTextOf(r agent.ToolResult) string {
	var b strings.Builder
	for _, blk := range r.Content {
		b.WriteString(blk.Text)
	}
	return b.String()
}

func TestWebFetchHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!DOCTYPE html><html><head><title>Test Page</title>
<style>body{color:red}</style></head><body>
<nav><a href="/menu">Menu</a></nav>
<script>var evil = 1;</script>
<h1>Hello World</h1>
<p>First paragraph.</p>
<p>Second <b>paragraph</b> with <a href="https://example.com/more">a link</a>.</p>
</body></html>`)
	}))
	defer srv.Close()

	tool := WebFetchTool{Config: allowPrivateConfig}
	res := tool.Execute(webToolContext(), json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultTextOf(res))
	}
	out := resultTextOf(res)
	for _, want := range []string{"Title: Test Page", "Hello World", "First paragraph.", "Second paragraph with a link"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "(https://example.com/more)") {
		t.Errorf("link href not preserved:\n%s", out)
	}
	for _, banned := range []string{"evil", "color:red"} {
		if strings.Contains(out, banned) {
			t.Errorf("output should not contain %q:\n%s", banned, out)
		}
	}
}

func TestWebFetchJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"scode","ok":true}`)
	}))
	defer srv.Close()

	tool := WebFetchTool{Config: allowPrivateConfig}
	res := tool.Execute(webToolContext(), json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultTextOf(res))
	}
	if !strings.Contains(resultTextOf(res), `"name":"scode"`) {
		t.Errorf("JSON body not passed through: %s", resultTextOf(res))
	}
}

func TestWebFetchBinaryRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte{0x00, 0x01, 0x02})
	}))
	defer srv.Close()

	tool := WebFetchTool{Config: allowPrivateConfig}
	res := tool.Execute(webToolContext(), json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if !res.IsError {
		t.Fatal("binary content should be rejected")
	}
	if !strings.Contains(resultTextOf(res), "application/octet-stream") {
		t.Errorf("error should name the content type: %s", resultTextOf(res))
	}
}

func TestWebFetchBlocksPrivateNetworkByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "secret")
	}))
	defer srv.Close()

	// Zero-value config (the shipped default) must refuse the loopback
	// test server; opting in must succeed.
	blocked := WebFetchTool{}.Execute(webToolContext(), json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if !blocked.IsError {
		t.Fatal("private network should be blocked by default")
	}
	if !strings.Contains(resultTextOf(blocked), "private") {
		t.Errorf("error should explain the private-network refusal: %s", resultTextOf(blocked))
	}

	allowed := WebFetchTool{Config: allowPrivateConfig}.Execute(webToolContext(), json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if allowed.IsError {
		t.Fatalf("allowPrivateNetwork should permit loopback: %s", resultTextOf(allowed))
	}
	if !strings.Contains(resultTextOf(allowed), "secret") {
		t.Errorf("expected body, got: %s", resultTextOf(allowed))
	}
}

func TestWebFetchRedirect(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "final destination")
	}))
	defer srv.Close()

	tool := WebFetchTool{Config: allowPrivateConfig}
	res := tool.Execute(webToolContext(), json.RawMessage(`{"url":"`+srv.URL+`/start"}`))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultTextOf(res))
	}
	out := resultTextOf(res)
	if !strings.Contains(out, "final destination") {
		t.Errorf("redirect target content missing: %s", out)
	}
	if !strings.Contains(out, "[redirected to ") {
		t.Errorf("redirect notice missing: %s", out)
	}
}

func TestWebFetchTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		for i := 0; i < MaxLines+500; i++ {
			fmt.Fprintf(w, "line %d\n", i)
		}
	}))
	defer srv.Close()

	tool := WebFetchTool{Config: allowPrivateConfig}
	res := tool.Execute(webToolContext(), json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultTextOf(res))
	}
	if !strings.Contains(resultTextOf(res), "[Showing lines 1-") {
		t.Errorf("truncation notice missing")
	}
}

func TestWebFetchBadArgs(t *testing.T) {
	tool := WebFetchTool{Config: allowPrivateConfig}
	for _, args := range []string{`{}`, `{"url":"ftp://x"}`, `{"url":"  "}`} {
		if res := tool.Execute(webToolContext(), json.RawMessage(args)); !res.IsError {
			t.Errorf("args %s should fail", args)
		}
	}
}

func TestWebFetchSchemeCompletion(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	// "host:port/path" without a scheme is normalized to https://… —
	// which fails against the plain-HTTP test server, proving the
	// completion happened (the error must be a fetch error, not an
	// argument error).
	noScheme := strings.TrimPrefix(srv.URL, "http://")
	res := WebFetchTool{Config: allowPrivateConfig}.Execute(webToolContext(), json.RawMessage(`{"url":"`+noScheme+`"}`))
	if !res.IsError {
		t.Fatal("https against a plain-http server should fail")
	}
	if strings.Contains(resultTextOf(res), "url is required") || strings.Contains(resultTextOf(res), "missing host") {
		t.Errorf("scheme completion did not happen: %s", resultTextOf(res))
	}
}

func TestWebSearchDuckDuckGoParse(t *testing.T) {
	page := `<html><body>
<div class="result results_links">
  <div class="result__title"><a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fone&amp;rut=abc">First Result</a></div>
  <a class="result__snippet">Snippet one.</a>
</div>
<div class="result results_links">
  <div class="result__title"><a class="result__a" href="https://example.com/two">Second Result</a></div>
  <a class="result__snippet">Snippet two.</a>
</div>
</body></html>`
	results := parseDDGResults(page, 10)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2: %+v", len(results), results)
	}
	if results[0].Title != "First Result" || results[0].URL != "https://example.com/one" {
		t.Errorf("result 0 wrong: %+v", results[0])
	}
	if results[0].Snippet != "Snippet one." {
		t.Errorf("snippet 0 wrong: %+v", results[0])
	}
	if results[1].URL != "https://example.com/two" {
		t.Errorf("direct link should pass through: %+v", results[1])
	}
}

func TestWebSearchDuckDuckGoEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "golang" {
			t.Errorf("query not forwarded: %s", r.URL)
		}
		fmt.Fprint(w, `<html><body><div class="result__title">
<a class="result__a" href="https://go.dev">The Go Programming Language</a></div>
<a class="result__snippet">Go is an open source language.</a></body></html>`)
	}))
	defer srv.Close()
	old := ddgSearchURL
	ddgSearchURL = srv.URL
	defer func() { ddgSearchURL = old }()

	tool := WebSearchTool{} // zero config → duckduckgo
	res := tool.Execute(webToolContext(), json.RawMessage(`{"query":"golang"}`))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultTextOf(res))
	}
	out := resultTextOf(res)
	for _, want := range []string{"1. The Go Programming Language", "https://go.dev", "open source"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestWebSearchBrave(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Subscription-Token") != "test-key" {
			t.Error("API key header missing")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"web":{"results":[{"title":"Brave Hit","url":"https://brave.example","description":"brave snippet"}]}}`)
	}))
	defer srv.Close()
	old := braveSearchURL
	braveSearchURL = srv.URL
	defer func() { braveSearchURL = old }()

	tool := WebSearchTool{Config: func() WebConfig {
		return WebConfig{SearchProvider: "brave", BraveAPIKey: "test-key"}
	}}
	res := tool.Execute(webToolContext(), json.RawMessage(`{"query":"q"}`))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultTextOf(res))
	}
	if !strings.Contains(resultTextOf(res), "Brave Hit") {
		t.Errorf("brave result missing: %s", resultTextOf(res))
	}
}

func TestWebSearchTavily(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["api_key"] != "tv-key" {
			t.Errorf("api_key not posted: %v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[{"title":"Tavily Hit","url":"https://tavily.example","content":"tavily content"}]}`)
	}))
	defer srv.Close()
	old := tavilySearchURL
	tavilySearchURL = srv.URL
	defer func() { tavilySearchURL = old }()

	tool := WebSearchTool{Config: func() WebConfig {
		return WebConfig{SearchProvider: "tavily", TavilyAPIKey: "tv-key"}
	}}
	res := tool.Execute(webToolContext(), json.RawMessage(`{"query":"q","limit":5}`))
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultTextOf(res))
	}
	if !strings.Contains(resultTextOf(res), "Tavily Hit") {
		t.Errorf("tavily result missing: %s", resultTextOf(res))
	}
}

func TestWebSearchMissingKeyAndBadProvider(t *testing.T) {
	tool := WebSearchTool{Config: func() WebConfig { return WebConfig{SearchProvider: "brave"} }}
	if res := tool.Execute(webToolContext(), json.RawMessage(`{"query":"q"}`)); !res.IsError ||
		!strings.Contains(resultTextOf(res), "BRAVE_API_KEY") {
		t.Errorf("missing brave key should produce a helpful error: %+v", res)
	}
	tool = WebSearchTool{Config: func() WebConfig { return WebConfig{SearchProvider: "bing"} }}
	if res := tool.Execute(webToolContext(), json.RawMessage(`{"query":"q"}`)); !res.IsError ||
		!strings.Contains(resultTextOf(res), "unknown web.searchProvider") {
		t.Errorf("unknown provider should fail clearly: %+v", res)
	}
	if res := (WebSearchTool{}).Execute(webToolContext(), json.RawMessage(`{"query":"  "}`)); !res.IsError {
		t.Error("empty query should fail")
	}
}

func TestWebToolsInRegistry(t *testing.T) {
	r := NewCodingRegistry()
	names := map[string]bool{}
	for _, d := range r.Decls() {
		names[d.Name] = true
	}
	for _, want := range []string{"web_fetch", "web_search"} {
		if !names[want] {
			t.Errorf("%s missing from the coding registry", want)
		}
	}
	// Re-adding a configured instance must keep declaration order.
	before := fmt.Sprintf("%v", r.Decls())
	r.Add(WebFetchTool{Config: allowPrivateConfig})
	after := fmt.Sprintf("%v", r.Decls())
	if before != after {
		t.Errorf("re-adding web_fetch changed the declaration bytes")
	}
}
