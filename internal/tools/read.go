package tools

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"scode/internal/agent"
	"scode/internal/llm"
)

// ReadTool pages through files (pi read.ts schema: path / offset 1-based /
// limit), detects images by content, and appends continuation pointers
// so the model can navigate long files without guessing.
type ReadTool struct{}

func (ReadTool) Decl() llm.Tool {
	return llm.Tool{
		Name:        "read",
		Description: "Read a file's contents. Text files return a line window with continuation pointers ([Showing lines X-Y of N]); use offset/limit to page. Image files are returned as attachments.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"File path, absolute or relative to the working directory"},"offset":{"type":"integer","description":"1-based line number to start from"},"limit":{"type":"integer","description":"Maximum number of lines to return"}},"required":["path"]}`),
	}
}

func (ReadTool) Execute(tc agent.ToolContext, args json.RawMessage) agent.ToolResult {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return agent.ErrorResult("invalid arguments: " + err.Error())
	}
	if a.Path == "" {
		return agent.ErrorResult("path is required")
	}
	path := Resolve(tc, a.Path)

	data, err := os.ReadFile(path)
	if err != nil {
		return agent.ErrorResult(fmt.Sprintf("cannot read %s: %v", a.Path, err))
	}

	// Detect images by content, not by extension (pi's
	// detectSupportedImageMimeTypeFromFile). An empty or mislabeled file
	// must fall through to the text path: emitting an image block with no
	// bytes would be replayed in every later provider request and rejected
	// (Kimi: "unsupported image format: text/plain; charset=utf-8"),
	// bricking the whole session.
	if mime := llm.DetectImageMIME(data); mime != "" {
		return agent.ToolResult{Content: []llm.Block{
			llm.TextBlock(fmt.Sprintf("[Image %s, %d bytes]", a.Path, len(data))),
			{Kind: llm.BlockImage, MimeType: mime, Data: base64.StdEncoding.EncodeToString(data)},
		}}
	}

	text := string(data)
	lines := strings.Split(text, "\n")
	total := len(lines)
	if total > 0 && lines[total-1] == "" {
		total-- // trailing newline is not a line
	}
	if total == 0 {
		total = 1
	}

	start := 1
	if a.Offset > 1 {
		start = a.Offset
	}
	if start > total {
		start = total
	}
	end := total
	if a.Limit > 0 && start-1+a.Limit < end {
		end = start - 1 + a.Limit
	}
	window := strings.Join(lines[start-1:end], "\n")

	tr := TruncateHead(window)
	out := tr.Text
	if tr.Truncated {
		out += "\n" + tr.Notice
	}
	if start > 1 || end < total {
		out += fmt.Sprintf("\n[Showing lines %d-%d of %d. Use offset=%d to continue.]", start, end, total, end+1)
	}
	return agent.TextResult(out)
}

// PromptContribution is pi's readToolSystemPromptContribution.
func (ReadTool) PromptContribution() (string, []string) {
	return "Read file contents", []string{"Use read to examine files instead of cat or sed."}
}
