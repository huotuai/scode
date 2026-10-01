package tools

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// WebConfig carries the resolved settings.json "web" section plus env
// API keys for the web tools. The cli binds a closure over the live
// settings so edits apply mid-session; a nil accessor means the zero
// value (duckduckgo search, private network blocked).
type WebConfig struct {
	SearchProvider      string // "" = duckduckgo | brave | tavily
	BraveAPIKey         string // BRAVE_API_KEY
	TavilyAPIKey        string // TAVILY_API_KEY
	AllowPrivateNetwork bool   // web_fetch may reach loopback/private/metadata addresses
}

// searchProvider applies the default backend.
func (c WebConfig) searchProvider() string {
	if c.SearchProvider == "" {
		return "duckduckgo"
	}
	return c.SearchProvider
}

// resolveWebConfig reads the tool's config accessor (nil = defaults).
func resolveWebConfig(f func() WebConfig) WebConfig {
	if f == nil {
		return WebConfig{}
	}
	return f()
}

// Web-fetch limits: generous transport cap (docs bundles and big JSON
// payloads survive), with the final 50KB model-facing cut left to
// TruncateHead like every other tool.
const (
	webTimeout  = 30 * time.Second
	webMaxBytes = 8 << 20 // 8 MiB hard cap on fetched bodies
	webMaxRedir = 10
)

// webUserAgent identifies scode without tripping the bot filters that
// reject Go's default UA outright.
const webUserAgent = "Mozilla/5.0 (compatible; scode-webfetch/1.0; +https://github.com/njzhenghao/SCode)"

// newWebClient builds the web_fetch HTTP client. The SSRF guard lives
// in DialContext so redirect hops are checked too (Claude Code blocks
// private/metadata targets the same way): every connection's host is
// resolved and private/loopback/link-local addresses are refused
// unless the config opts in. The check-then-dial race (DNS rebinding)
// is accepted for v1 — the permission layer is the real gate.
func newWebClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if !allowPrivate {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
				if err != nil {
					return nil, fmt.Errorf("cannot resolve %s: %w", host, err)
				}
				for _, ip := range ips {
					if isBlockedIP(ip) {
						return nil, fmt.Errorf("refusing to connect to private/loopback/link-local address %s (%s); set web.allowPrivateNetwork in settings to override", ip, host)
					}
				}
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= webMaxRedir {
				return fmt.Errorf("stopped after %d redirects", webMaxRedir)
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("refusing redirect to non-HTTP scheme %q", req.URL.Scheme)
			}
			return nil
		},
	}
}

// isBlockedIP reports whether an address is off-limits for web_fetch by
// default: loopback, RFC1918/ULA private, link-local (covers the cloud
// metadata endpoint 169.254.169.254), CGNAT, multicast, unspecified.
func isBlockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		return v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 // CGNAT 100.64.0.0/10
	}
	return false
}

// readWebBody reads up to webMaxBytes+1, reporting whether the cap
// cut the stream short.
func readWebBody(r io.Reader) (body []byte, truncated bool, err error) {
	body, err = io.ReadAll(io.LimitReader(r, webMaxBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(body) > webMaxBytes {
		return body[:webMaxBytes], true, nil
	}
	return body, false, nil
}

// htmlToText converts an HTML document to readable plain text
// (Claude Code's WebFetch converts to markdown upstream; v1 keeps a
// lighter text rendering): script/style/noscript/template content and
// comments are dropped, block-level elements become line boundaries,
// links keep their href in parentheses, <pre> whitespace is preserved,
// and the document <title> leads the output.
func htmlToText(r io.Reader) (string, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	// The <title> is extracted first; <head> is then skipped whole.
	for title := range findElements(doc, "title") {
		if t := strings.TrimSpace(nodeText(title)); t != "" {
			b.WriteString("Title: " + t + "\n\n")
			break
		}
	}
	var walk func(n *html.Node, inPre bool)
	walk = func(n *html.Node, inPre bool) {
		if n.Type == html.CommentNode || n.Type == html.DoctypeNode {
			return
		}
		if n.Type == html.TextNode {
			if inPre {
				b.WriteString(n.Data)
			} else {
				b.WriteString(collapseSpaces(n.Data))
			}
			return
		}
		elem := n.Type == html.ElementNode
		if elem {
			switch n.Data {
			case "head", "script", "style", "noscript", "template":
				return
			}
			if isBlockTag(n.Data) {
				b.WriteString("\n")
			}
		}
		childPre := inPre || (elem && n.Data == "pre")
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, childPre)
		}
		if !elem {
			return
		}
		if n.Data == "a" {
			if href := attr(n, "href"); href != "" && !strings.HasPrefix(href, "#") {
				text := strings.TrimSpace(nodeText(n))
				if text != "" && !strings.Contains(text, href) {
					b.WriteString(" (" + href + ")")
				}
			}
		}
		if isBlockTag(n.Data) || n.Data == "br" || n.Data == "hr" {
			b.WriteString("\n")
		}
	}
	walk(doc, false)
	return tidyText(b.String()), nil
}

// findElements yields descendants (and the node itself) with the tag.
func findElements(n *html.Node, tag string) func(yield func(*html.Node) bool) {
	return func(yield func(*html.Node) bool) {
		var walk func(*html.Node) bool
		walk = func(m *html.Node) bool {
			if m.Type == html.ElementNode && m.Data == tag {
				if !yield(m) {
					return false
				}
			}
			for c := m.FirstChild; c != nil; c = c.NextSibling {
				if !walk(c) {
					return false
				}
			}
			return true
		}
		walk(n)
	}
}

// isBlockTag marks elements that break onto their own line.
func isBlockTag(tag string) bool {
	switch tag {
	case "p", "div", "section", "article", "aside", "header", "footer", "nav",
		"main", "figure", "figcaption", "blockquote", "pre", "form",
		"ul", "ol", "li", "dl", "dt", "dd", "table", "thead", "tbody",
		"tr", "td", "th", "h1", "h2", "h3", "h4", "h5", "h6":
		return true
	}
	return false
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// nodeText flattens a subtree's text (title extraction, link-label
// comparison).
func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(m *html.Node) {
		if m.Type == html.TextNode {
			b.WriteString(m.Data)
		}
		for c := m.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// collapseSpaces folds ASCII whitespace runs into single spaces.
func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ") + " "
}

// tidyText normalizes the extracted stream: trim lines, drop empties,
// cap blank runs at one.
func tidyText(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	blank := false
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, l)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
}

// decodeText wraps a body in charset detection (GBK and friends decode
// to UTF-8) using the Content-Type header as the first hint.
func decodeText(r io.Reader, contentType string) (io.Reader, error) {
	return charset.NewReader(r, contentType)
}

// normalizeFetchURL completes a scheme-less argument with https://.
func normalizeFetchURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("url is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q (http/https only)", u.Scheme)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("invalid url %q: missing host", raw)
	}
	return u.String(), nil
}
