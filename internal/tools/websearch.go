package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/html"

	"scode/internal/agent"
	"scode/internal/llm"
)

// WebSearchTool searches the web through a configurable backend
// (settings web.searchProvider): duckduckgo (default, zero-config —
// scrapes the html.duckduckgo.com lite endpoint, best-effort and
// brittle by nature), brave and tavily (API-key backends, keys from
// BRAVE_API_KEY / TAVILY_API_KEY). Endpoint URLs are package vars so
// tests can point them at httptest servers.
type WebSearchTool struct {
	// Config resolves the web settings per call; nil = defaults.
	Config func() WebConfig
}

func (WebSearchTool) Decl() llm.Tool {
	return llm.Tool{
		Name:        "web_search",
		Description: "Search the web and return a numbered list of results (title, URL, snippet). Use web_fetch to read a result's full page. The backend is configurable via settings web.searchProvider (duckduckgo default; brave/tavily need BRAVE_API_KEY / TAVILY_API_KEY).",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"The search query"},"limit":{"type":"integer","description":"Maximum results to return (default 10, max 20)"}},"required":["query"]}`),
	}
}

const (
	webSearchDefaultLimit = 10
	webSearchMaxLimit     = 20
)

// Backend endpoint URLs (package vars: tests point them at httptest
// servers).
var (
	ddgSearchURL    = "https://html.duckduckgo.com/html/"
	braveSearchURL  = "https://api.search.brave.com/res/v1/web/search"
	tavilySearchURL = "https://api.tavily.com/search"
)

// webResult is one search hit, backend-neutral.
type webResult struct {
	Title   string
	URL     string
	Snippet string
}

func (t WebSearchTool) Execute(tc agent.ToolContext, args json.RawMessage) agent.ToolResult {
	var a struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return agent.ErrorResult("invalid arguments: " + err.Error())
	}
	query := strings.TrimSpace(a.Query)
	if query == "" {
		return agent.ErrorResult("query is required")
	}
	limit := a.Limit
	if limit <= 0 {
		limit = webSearchDefaultLimit
	}
	if limit > webSearchMaxLimit {
		limit = webSearchMaxLimit
	}
	cfg := resolveWebConfig(t.Config)
	if tc.Progress != nil {
		tc.Progress(fmt.Sprintf("searching %q via %s", query, cfg.searchProvider()))
	}

	ctx, cancel := context.WithTimeout(tc.Ctx, webTimeout)
	defer cancel()

	var results []webResult
	var err error
	switch p := cfg.searchProvider(); p {
	case "duckduckgo":
		results, err = searchDuckDuckGo(ctx, query, limit)
	case "brave":
		if cfg.BraveAPIKey == "" {
			return agent.ErrorResult(`web_search provider "brave" needs an API key: set the BRAVE_API_KEY environment variable (or switch web.searchProvider to "duckduckgo")`)
		}
		results, err = searchBrave(ctx, query, limit, cfg.BraveAPIKey)
	case "tavily":
		if cfg.TavilyAPIKey == "" {
			return agent.ErrorResult(`web_search provider "tavily" needs an API key: set the TAVILY_API_KEY environment variable (or switch web.searchProvider to "duckduckgo")`)
		}
		results, err = searchTavily(ctx, query, limit, cfg.TavilyAPIKey)
	default:
		return agent.ErrorResult(fmt.Sprintf("unknown web.searchProvider %q (want duckduckgo, brave, or tavily)", p))
	}
	if err != nil {
		return agent.ErrorResult(fmt.Sprintf("web_search failed: %v", err))
	}
	if len(results) == 0 {
		return agent.TextResult(fmt.Sprintf("No results found for %q.", query))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Search results for %q:\n\n", query)
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if r.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", r.Snippet)
		}
	}
	tr := TruncateHead(b.String())
	out := tr.Text
	if tr.Truncated {
		out += "\n" + tr.Notice
	}
	return agent.TextResult(out)
}

// searchHTTP is the client for the search backends: plain (no SSRF
// guard — endpoints are hard-coded public hosts, not model input).
var searchHTTP = &http.Client{Timeout: webTimeout}

// searchDuckDuckGo scrapes the DuckDuckGo HTML endpoint (no API key).
// The markup is not a stable contract; parsing is deliberately
// forgiving (zip title anchors with snippet blocks by document order).
func searchDuckDuckGo(ctx context.Context, query string, limit int) ([]webResult, error) {
	u := ddgSearchURL + "?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", webUserAgent)
	resp, err := searchHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("duckduckgo: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, webMaxBytes))
	if err != nil {
		return nil, err
	}
	return parseDDGResults(string(body), limit), nil
}

// parseDDGResults extracts results from the html.duckduckgo.com
// response: anchors with class "result__a" carry title + (proxied)
// href, elements with class "result__snippet" the abstract. Hrefs go
// through duckduckgo.com/l/?uddg=<url-encoded target>; the real target
// is unwrapped.
func parseDDGResults(page string, limit int) []webResult {
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return nil
	}
	var titles []*html.Node
	var snippets []string
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			class := attr(n, "class")
			if n.Data == "a" && strings.Contains(class, "result__a") {
				titles = append(titles, n)
			} else if strings.Contains(class, "result__snippet") {
				snippets = append(snippets, strings.TrimSpace(nodeText(n)))
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	var out []webResult
	for i, a := range titles {
		if len(out) >= limit {
			break
		}
		r := webResult{
			Title: strings.TrimSpace(nodeText(a)),
			URL:   unwrapDDGLink(attr(a, "href")),
		}
		if r.URL == "" {
			continue
		}
		if i < len(snippets) {
			r.Snippet = snippets[i]
		}
		out = append(out, r)
	}
	return out
}

// unwrapDDGLink resolves the DuckDuckGo redirector (/l/?uddg=...) to
// the real target; protocol-relative links get a scheme.
func unwrapDDGLink(href string) string {
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if u.Host == "duckduckgo.com" && strings.HasPrefix(u.Path, "/l/") {
		if target := u.Query().Get("uddg"); target != "" {
			return target
		}
		return ""
	}
	return href
}

// searchBrave calls the Brave Search API (X-Subscription-Token auth).
func searchBrave(ctx context.Context, query string, limit int, key string) ([]webResult, error) {
	u := fmt.Sprintf("%s?q=%s&count=%d", braveSearchURL, url.QueryEscape(query), limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", key)
	req.Header.Set("User-Agent", webUserAgent)
	resp, err := searchHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, webMaxBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("brave: HTTP %d: %s", resp.StatusCode, truncateRunes(string(body), 200))
	}
	var parsed struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("brave: invalid response: %w", err)
	}
	out := make([]webResult, 0, len(parsed.Web.Results))
	for _, r := range parsed.Web.Results {
		out = append(out, webResult{Title: r.Title, URL: r.URL, Snippet: r.Description})
	}
	return out, nil
}

// searchTavily calls the Tavily Search API (key in the JSON body).
func searchTavily(ctx context.Context, query string, limit int, key string) ([]webResult, error) {
	payload, err := json.Marshal(map[string]any{
		"api_key":     key,
		"query":       query,
		"max_results": limit,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tavilySearchURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", webUserAgent)
	resp, err := searchHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, webMaxBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tavily: HTTP %d: %s", resp.StatusCode, truncateRunes(string(body), 200))
	}
	var parsed struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("tavily: invalid response: %w", err)
	}
	out := make([]webResult, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		out = append(out, webResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return out, nil
}

// truncateRunes caps one error-message excerpt (rune-safe).
func truncateRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

// PromptContribution is the web_search system-prompt snippet.
func (WebSearchTool) PromptContribution() (string, []string) {
	return "Search the web (results as title/URL/snippet; read pages with web_fetch)", nil
}
