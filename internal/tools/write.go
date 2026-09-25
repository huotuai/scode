package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"scode/internal/agent"
	"scode/internal/llm"
)

// WriteTool creates or overwrites files whole (pi write.ts schema:
// path / content), creating parent directories, serialized per path.
type WriteTool struct{}

func (WriteTool) Decl() llm.Tool {
	return llm.Tool{
		Name:        "write",
		Description: "Write a file's full contents, creating parent directories as needed. Overwrites existing files.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"File path, absolute or relative to the working directory"},"content":{"type":"string","description":"Full file content to write"}},"required":["path","content"]}`),
	}
}

func (WriteTool) Execute(tc agent.ToolContext, args json.RawMessage) agent.ToolResult {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return agent.ErrorResult("invalid arguments: " + err.Error())
	}
	if a.Path == "" {
		return agent.ErrorResult("path is required")
	}
	path := Resolve(tc, a.Path)

	err := WithFileMutation(path, func() error {
		if dir := filepath.Dir(path); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("create directory: %w", err)
			}
		}
		info, err := os.Stat(path)
		if err == nil && info.IsDir() {
			return fmt.Errorf("%s is a directory", a.Path)
		}
		return os.WriteFile(path, []byte(a.Content), 0o644)
	})
	if err != nil {
		return agent.ErrorResult(fmt.Sprintf("cannot write %s: %v", a.Path, err))
	}
	return agent.TextResult(fmt.Sprintf("Wrote %d bytes to %s", len(a.Content), a.Path))
}
