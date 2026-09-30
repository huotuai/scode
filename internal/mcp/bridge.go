package mcp

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"scode/internal/llm"
)

// ---------------------------------------------------------------------------
// Public tool naming (dsh's publicToolName)
// ---------------------------------------------------------------------------

// maxPublicNameLength is the function-name wire budget (dsh's DeepSeek
// contract: 64 chars).
const maxPublicNameLength = 64

// hashLength is the SHA-256 hex suffix appended on lossy normalization.
const hashLength = 12

var invalidNameChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// PublicName derives the model-facing tool name for one MCP tool (dsh's
// publicToolName): mcp__<server>__<raw> verbatim when it fits the
// function-name contract; otherwise normalized, truncated, and disambiguated
// with a 12-hex SHA-256 of the identity so distinct tools never collapse.
// The raw name is only ever sent on the wire (tools/call).
func PublicName(server, raw string) string {
	joined := "mcp__" + server + "__" + raw
	normalized := invalidNameChars.ReplaceAllString(joined, "_")
	if normalized == joined && len(normalized) <= maxPublicNameLength {
		return normalized
	}
	sum := sha256.Sum256([]byte(server + "\x00" + raw))
	hash := hex.EncodeToString(sum[:])[:hashLength]
	keep := maxPublicNameLength - hashLength - 1
	if len(normalized) > keep {
		normalized = normalized[:keep]
	}
	return normalized + "_" + hash
}

// ---------------------------------------------------------------------------
// stdio child environment (dsh's scrubbedParentEnv)
// ---------------------------------------------------------------------------

// sensitiveEnvPattern marks credential-shaped names that must not leak
// into spawned servers implicitly (dsh's SENSITIVE_ENV_PATTERN).
var sensitiveEnvPattern = regexp.MustCompile(`(?i)KEY|PASSWORD|SECRET|TOKEN`)

// childEnv builds the environment for a stdio server: the ambient
// environment minus credential-shaped names and minus all SCODE_* names
// (the harness's own facts never leak implicitly), with the config's
// explicit env merged AFTER the scrub so a deliberately supplied entry
// survives (dsh's buildChildEnv).
func childEnv(extra map[string]string) []string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if sensitiveEnvPattern.MatchString(k) || strings.HasPrefix(strings.ToUpper(k), "SCODE_") {
			continue
		}
		env[k] = v
	}
	for k, v := range extra {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// ---------------------------------------------------------------------------
// Content projection (dsh's projectContent / extractText)
// ---------------------------------------------------------------------------

// imageMediaTypes is the durable raster vocabulary (dsh's IMAGE_MEDIA_TYPES).
var imageMediaTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

// projectContent converts MCP result content into scode blocks (dsh's
// projectContent): text runs newline-coalesce; images split the runs at
// their original position and enter as image blocks when their media type
// is in the durable vocabulary (providers gate forwarding on model
// capability at the wire boundary — scode's existing convention, so the
// bridge always forwards valid images); unsupported blocks become
// explicit placeholder text.
func projectContent(content []mcpsdk.Content, toolName string) []llm.Block {
	var projected []llm.Block
	var text []string
	flush := func() {
		if len(text) > 0 {
			projected = append(projected, llm.TextBlock(strings.Join(text, "\n")))
			text = nil
		}
	}

	for _, c := range content {
		switch c := c.(type) {
		case *mcpsdk.TextContent:
			text = append(text, c.Text)
		case *mcpsdk.ImageContent:
			flush()
			if !imageMediaTypes[c.MIMEType] {
				projected = append(projected, llm.TextBlock(
					fmt.Sprintf("[image unavailable: %s; the declared media type is not PNG, JPEG, WebP, or GIF]", orUnknown(c.MIMEType))))
				continue
			}
			projected = append(projected, llm.Block{
				Kind:     llm.BlockImage,
				MimeType: c.MIMEType,
				Data:     base64.StdEncoding.EncodeToString(c.Data),
			})
		case *mcpsdk.ResourceLink:
			if c.Name == "" || c.URI == "" {
				text = append(text, "[resource link unavailable: the MCP block is missing its name or URI]")
			} else {
				text = append(text, fmt.Sprintf("Resource link: %s (%s)", c.Name, c.URI))
			}
		case *mcpsdk.AudioContent:
			text = append(text, fmt.Sprintf("[audio result unsupported: %s]", orUnknown(c.MIMEType)))
		case *mcpsdk.EmbeddedResource:
			text = append(text, "[embedded resource unsupported]")
		default:
			text = append(text, fmt.Sprintf("[unsupported MCP content type: %T]", c))
		}
	}
	flush()
	if len(projected) == 0 {
		projected = append(projected, llm.TextBlock(fmt.Sprintf("(%s returned no model-visible content)", toolName)))
	}
	return projected
}

// extractText renders result content as one string for error paths
// (dsh's extractText): texts join with newlines, rich blocks become
// placeholders.
func extractText(content []mcpsdk.Content, toolName string) string {
	var parts []string
	for _, b := range projectContent(content, toolName) {
		if b.Kind == llm.BlockText {
			parts = append(parts, b.Text)
		} else {
			parts = append(parts, fmt.Sprintf("[image: %s]", b.MimeType))
		}
	}
	return strings.Join(parts, "\n")
}

func orUnknown(mime string) string {
	if mime == "" {
		return "unknown media type"
	}
	return mime
}
