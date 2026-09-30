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
		Description: "Write a file's full contents, creating parent directories as needed. Overwrites existing files. When the file sandbox confines this session, a write outside the boundary may be retried ONCE with sandbox_permissions (the narrowest wider mode that suffices) + justification; the approval prompt asks the user.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"File path, absolute or relative to the working directory"},"content":{"type":"string","description":"Full file content to write"},"sandbox_permissions":{"type":"string","enum":["workspace-write","danger-full-access"],"description":"Sandbox escalation target for this call (requires justification)"},"justification":{"type":"string","description":"One-sentence reason for the sandbox escalation, shown to the user"}},"required":["path","content"]}`),
	}
}

func (WriteTool) Execute(tc agent.ToolContext, args json.RawMessage) agent.ToolResult {
	var a struct {
		Path               string `json:"path"`
		Content            string `json:"content"`
		SandboxPermissions string `json:"sandbox_permissions"`
		Justification      string `json:"justification"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return agent.ErrorResult("invalid arguments: " + err.Error())
	}
	if a.Path == "" {
		return agent.ErrorResult("path is required")
	}
	policy, escErr := ResolvePolicy(tc, a.SandboxPermissions, a.Justification, Resolve(tc, a.Path))
	if escErr != nil {
		return *escErr
	}
	path, err := FenceWrite(policy, tc, a.Path)
	if err != nil {
		return SandboxError(err, "operation")
	}

	err = WithFileMutation(path, func() error {
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

// PromptContribution is pi's writeToolSystemPromptContribution.
func (WriteTool) PromptContribution() (string, []string) {
	return "Create or overwrite files", []string{"Use write only for new files or complete rewrites."}
}
