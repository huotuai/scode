package cli

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"

	"scode/internal/llm"
)

// Prompt image attachments.
//
// Two sources: explicit blocks (the TUI's clipboard attachments) and
// image file paths typed/pasted/dragged into the prompt text (a
// terminal drag-drop lands as a quoted path). Path scanning is
// extension-gated for cost, but the final decision is always content
// sniffing (llm.DetectImageMIME) — a mislabeled or empty file must not
// become an image block, or every later provider request is rejected
// and the session bricks (see read.go).

// maxImageBytes caps one attachment's raw payload; base64 inflates it
// by ~4/3 on the wire and provider limits start around 5 MB.
const maxImageBytes = 10 << 20

// imageExts gates which prompt tokens are worth reading from disk.
var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".webp": true, ".bmp": true,
}

// ImageBlock builds an image content block from raw bytes, or reports
// false when the payload is empty, oversize, or not a supported image.
func ImageBlock(data []byte) (llm.Block, bool) {
	if len(data) == 0 || len(data) > maxImageBytes {
		return llm.Block{}, false
	}
	mime := llm.DetectImageMIME(data)
	if mime == "" {
		return llm.Block{}, false
	}
	return llm.Block{Kind: llm.BlockImage, MimeType: mime, Data: base64.StdEncoding.EncodeToString(data)}, true
}

// ImagePathRef pairs a raw prompt token with the image file it resolved
// to, so callers can both attach the file and strip the token from the
// text it appeared in (the TUI's paste interception).
type ImagePathRef struct {
	Token string // the token as it appeared (quotes stripped)
	Path  string // resolved absolute path
}

// ExtractImageRefs finds readable image file references in text and
// returns token+path pairs. Tokens may be absolute, relative to cwd,
// ~/-prefixed, or single/double/backtick quoted (terminal drag-drop
// quoting). Missing files, directories, and non-image extensions are
// skipped. ExtractImagePaths drops the tokens and returns just paths.
func ExtractImageRefs(text, cwd string) []ImagePathRef {
	var out []ImagePathRef
	seen := map[string]bool{}
	for _, tok := range tokenize(text) {
		if !imageExts[strings.ToLower(filepath.Ext(tok))] {
			continue
		}
		p := resolveImagePath(tok, cwd)
		key := filepath.Clean(p)
		if seen[key] {
			continue
		}
		info, err := os.Stat(p)
		if err != nil || info.IsDir() || info.Size() == 0 || info.Size() > maxImageBytes {
			continue
		}
		seen[key] = true
		out = append(out, ImagePathRef{Token: tok, Path: p})
	}
	return out
}

// ExtractImagePaths is ExtractImageRefs reduced to the resolved paths.
func ExtractImagePaths(text, cwd string) []string {
	refs := ExtractImageRefs(text, cwd)
	paths := make([]string, 0, len(refs))
	for _, r := range refs {
		paths = append(paths, r.Path)
	}
	return paths
}

// ScanImagePaths finds image file references in the raw prompt text and
// returns one image block per distinct readable file (content sniffing
// makes the final call — a mislabeled or empty file must not become an
// image block). Non-path tokens, missing files, and non-image content
// are silently skipped — the text stays verbatim for the model either
// way.
func ScanImagePaths(text, cwd string) []llm.Block {
	var out []llm.Block
	for _, p := range ExtractImagePaths(text, cwd) {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if block, ok := ImageBlock(data); ok {
			out = append(out, block)
		}
	}
	return out
}

// resolveImagePath expands ~ and makes p absolute relative to cwd
// (mirrors the skills resolver, minus the trim: tokens are clean).
func resolveImagePath(p, cwd string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(cwd, p)
}

// tokenize splits on whitespace, keeping single/double-quoted segments
// together and stripping the quotes (backticks strip too). A Windows
// path's backslashes are NOT escape characters here.
func tokenize(text string) []string {
	var toks []string
	var cur strings.Builder
	var quote rune // 0, '"', or '\''
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, strings.Trim(cur.String(), "`"))
			cur.Reset()
		}
	}
	for _, r := range text {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return toks
}
