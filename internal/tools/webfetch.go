package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"scode/internal/agent"
	"scode/internal/llm"
)

// WebFetchTool fetches one URL and returns its content as text (Claude
// Code's WebFetch, url-only variant: no secondary prompt pass — scode
// has no cheap side model, the main model reads the extracted text).
// HTML is rendered to readable text; JSON/XML/plain bodies pass
// through; binary content is refused with a hint to use bash curl.
// Private/loopback/metadata targets are blocked unless settings
// web.allowPrivateNetwork opts in (the SSRF guard runs per dial, so
// redirect hops are covered).
type WebFetchTool struct {
	// Config resolves the web settings per call; nil = defaults.
	Config func() WebConfig
}

func (WebFetchTool) Decl() llm.Tool {
	return llm.Tool{
		Name:        "web_fetch",
		Description: "Fetch a URL over HTTP/HTTPS and return its content as text. HTML pages are converted to readable text (scripts/styles stripped, links kept); JSON/XML/plain text is returned as-is; binary content is rejected (use bash curl for those). Private-network, loopback and cloud-metadata addresses are refused by default. Output is truncated to 50KB.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"The URL to fetch (http or https; https:// is assumed when omitted)"}},"required":["url"]}`),
	}
}

func (t WebFetchTool) Execute(tc agent.ToolContext, args json.RawMessage) agent.ToolResult {
	var a struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return agent.ErrorResult("invalid arguments: " + err.Error())
	}
	raw, err := normalizeFetchURL(a.URL)
	if err != nil {
		return agent.ErrorResult(err.Error())
	}
	cfg := resolveWebConfig(t.Config)
	if tc.Progress != nil {
		tc.Progress("GET " + raw)
	}

	ctx, cancel := context.WithTimeout(tc.Ctx, webTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return agent.ErrorResult("invalid url: " + err.Error())
	}
	req.Header.Set("User-Agent", webUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json,text/plain,*/*;q=0.8")

	resp, err := newWebClient(cfg.AllowPrivateNetwork).Do(req)
	if err != nil {
		return agent.ErrorResult(fmt.Sprintf("fetch %s failed: %v", raw, err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return agent.ErrorResult(fmt.Sprintf("fetch %s: HTTP %d %s", raw, resp.StatusCode, http.StatusText(resp.StatusCode)))
	}

	ct := resp.Header.Get("Content-Type")
	body, capped, err := readWebBody(resp.Body)
	if err != nil {
		return agent.ErrorResult(fmt.Sprintf("fetch %s: reading body failed: %v", raw, err))
	}

	var text string
	if isHTMLContent(ct) {
		r, err := decodeText(strings.NewReader(string(body)), ct)
		if err == nil {
			text, err = htmlToText(r)
		}
		if err != nil {
			return agent.ErrorResult(fmt.Sprintf("fetch %s: HTML extraction failed: %v", raw, err))
		}
	} else if isTextContent(ct) {
		r, err := decodeText(strings.NewReader(string(body)), ct)
		if err != nil {
			return agent.ErrorResult(fmt.Sprintf("fetch %s: charset decoding failed: %v", raw, err))
		}
		b, err := io.ReadAll(r)
		if err != nil {
			return agent.ErrorResult(fmt.Sprintf("fetch %s: reading body failed: %v", raw, err))
		}
		text = string(b)
	} else {
		return agent.ErrorResult(fmt.Sprintf("fetch %s: unsupported content type %q (%d bytes) — only text/HTML/JSON/XML can be returned; use bash curl to download binary content", raw, ct, len(body)))
	}

	var head strings.Builder
	if resp.Request.URL.String() != raw {
		fmt.Fprintf(&head, "[redirected to %s]\n", resp.Request.URL.String())
	}
	if capped {
		fmt.Fprintf(&head, "[response body capped at %d bytes]\n", webMaxBytes)
	}
	head.WriteString(text)
	tr := TruncateHead(head.String())
	out := tr.Text
	if tr.Truncated {
		out += "\n" + tr.Notice
	}
	return agent.TextResult(out)
}

// isHTMLContent reports whether the body should go through the HTML
// text extractor.
func isHTMLContent(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml")
}

// isTextContent reports whether the body can be returned as decoded
// text (a missing Content-Type gets the benefit of the doubt — many
// small servers omit it for plain text).
func isTextContent(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if ct == "" {
		return true
	}
	if strings.HasPrefix(ct, "text/") {
		return true
	}
	for _, t := range []string{"application/json", "application/xml", "application/javascript",
		"application/x-javascript", "application/yaml", "application/x-yaml",
		"application/toml", "application/sql", "application/graphql",
		"application/rss+xml", "application/atom+xml", "image/svg+xml"} {
		if strings.HasPrefix(ct, t) {
			return true
		}
	}
	return strings.HasSuffix(ct, "+json") || strings.HasSuffix(ct, "+xml")
}

// PromptContribution is the web_fetch system-prompt snippet.
func (WebFetchTool) PromptContribution() (string, []string) {
	return "Fetch a URL and return its content as text (HTML converted to readable text)", nil
}
