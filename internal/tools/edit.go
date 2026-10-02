package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"scode/internal/agent"
	"scode/internal/llm"
)

// EditTool applies targeted string replacements (pi edit.ts schema:
// path / edits[]{oldText,newText}). Malformed model output is repaired
// where unambiguous (edits as JSON string, single object, flat
// oldText/newText), matching pi's prepareArguments.
type EditTool struct{}

func (EditTool) Decl() llm.Tool {
	return llm.Tool{
		Name:        "edit",
		Description: "Edit a file by replacing exact text blocks. Each edit's oldText must match the file uniquely; provide more surrounding context when it does not. Multiple edits apply against the original content and must not overlap. When the file sandbox confines this session, an edit outside the boundary may be retried ONCE with sandbox_permissions (the narrowest wider mode that suffices) + justification; the approval prompt asks the user.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"File path, absolute or relative to the working directory"},"edits":{"type":"array","items":{"type":"object","properties":{"oldText":{"type":"string","description":"Exact text to find (must be unique in the file)"},"newText":{"type":"string","description":"Replacement text"}},"required":["oldText","newText"]}},"sandbox_permissions":{"type":"string","enum":["workspace-write","danger-full-access"],"description":"Sandbox escalation target for this call (requires justification)"},"justification":{"type":"string","description":"One-sentence reason for the sandbox escalation, shown to the user"}},"required":["path","edits"]}`),
	}
}

// parseEditArgs repairs the common model mistakes pi handles.
func parseEditArgs(args json.RawMessage) (path string, edits []editRequest, esc escalationArgs, err error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(args, &raw); err != nil {
		return "", nil, esc, err
	}
	if p, ok := raw["path"]; ok {
		_ = json.Unmarshal(p, &path)
	}
	if v, ok := raw["sandbox_permissions"]; ok {
		_ = json.Unmarshal(v, &esc.permissions)
	}
	if v, ok := raw["justification"]; ok {
		_ = json.Unmarshal(v, &esc.justification)
	}

	type editJSON struct {
		OldText string `json:"oldText"`
		NewText string `json:"newText"`
	}
	var list []editJSON

	if e, ok := raw["edits"]; ok {
		if err := json.Unmarshal(e, &list); err != nil {
			// Repairs: edits passed as a JSON string, or a single object.
			var s string
			if json.Unmarshal(e, &s) == nil {
				if json.Unmarshal([]byte(s), &list) != nil {
					return "", nil, esc, fmt.Errorf("edits must be an array of {oldText,newText}")
				}
			} else {
				var one editJSON
				if json.Unmarshal(e, &one) != nil || one.OldText == "" {
					return "", nil, esc, fmt.Errorf("edits must be an array of {oldText,newText}")
				}
				list = []editJSON{one}
			}
		}
	} else if o, ok := raw["oldText"]; ok {
		// Legacy flat form.
		one := editJSON{}
		_ = json.Unmarshal(o, &one.OldText)
		if n, ok := raw["newText"]; ok {
			_ = json.Unmarshal(n, &one.NewText)
		}
		list = []editJSON{one}
	}
	if path == "" {
		return "", nil, esc, fmt.Errorf("path is required")
	}
	if len(list) == 0 {
		return "", nil, esc, fmt.Errorf("edits must contain at least one {oldText,newText}")
	}
	for _, e := range list {
		edits = append(edits, editRequest{oldText: e.OldText, newText: e.NewText})
	}
	return path, edits, esc, nil
}

func (EditTool) Execute(tc agent.ToolContext, args json.RawMessage) agent.ToolResult {
	path, edits, esc, err := parseEditArgs(args)
	if err != nil {
		return agent.ErrorResult("invalid arguments: " + err.Error())
	}
	policy, escErr := ResolvePolicy(tc, esc.permissions, esc.justification, Resolve(tc, path))
	if escErr != nil {
		return *escErr
	}
	full, err := FenceWrite(policy, tc, path)
	if err != nil {
		return SandboxError(err, "operation")
	}

	var output string
	err = WithFileMutation(full, func() error {
		data, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		original := string(data)
		le := detectLineEnding(original)
		bom := ""
		if strings.HasPrefix(original, "\uFEFF") {
			bom = "\uFEFF" // preserve; normalizeToLF strips it
		}
		content := normalizeToLF(original)

		out, err := applyEdits(content, edits)
		if err != nil {
			return err
		}
		final := bom + restoreLineEndings(out, le)
		if err := os.WriteFile(full, []byte(final), 0o644); err != nil {
			return err
		}
		output = fmt.Sprintf("Successfully replaced %d block(s) in %s\n\n%s", len(edits), path, UnifiedDiff(content, out, path))
		return nil
	})
	if err != nil {
		return agent.ErrorResult(fmt.Sprintf("edit %s failed: %v", path, err))
	}
	return agent.TextResult(output)
}

// UnifiedDiff renders a line diff (LCS on the changed middle after
// squeezing common prefix/suffix). Large middles fall back to a summary.
func UnifiedDiff(a, b, path string) string {
	al := strings.Split(a, "\n")
	bl := strings.Split(b, "\n")

	p := 0
	for p < len(al) && p < len(bl) && al[p] == bl[p] {
		p++
	}
	s := 0
	for s < len(al)-p && s < len(bl)-p && al[len(al)-1-s] == bl[len(bl)-1-s] {
		s++
	}
	am := al[p : len(al)-s]
	bm := bl[p : len(bl)-s]

	if len(am) == 0 && len(bm) == 0 {
		return ""
	}
	const maxMiddle = 4000
	if len(am)+len(bm) > maxMiddle {
		return fmt.Sprintf("diff omitted (%d removed / %d added lines)", len(am), len(bm))
	}

	var sb strings.Builder
	sb.WriteString("--- " + path + "\n+++ " + path + "\n")
	sb.WriteString(fmt.Sprintf("@@ -%d,%d +%d,%d @@\n", p+1, len(am), p+1, len(bm)))

	// LCS table over the middle.
	n, m := len(am), len(bm)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if am[i] == bm[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case am[i] == bm[j]:
			sb.WriteString(" " + am[i] + "\n")
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			sb.WriteString("-" + am[i] + "\n")
			i++
		default:
			sb.WriteString("+" + bm[j] + "\n")
			j++
		}
	}
	for ; i < n; i++ {
		sb.WriteString("-" + am[i] + "\n")
	}
	for ; j < m; j++ {
		sb.WriteString("+" + bm[j] + "\n")
	}
	return sb.String()
}

// PromptContribution is pi's editToolSystemPromptContribution.
func (EditTool) PromptContribution() (string, []string) {
	return "Make precise file edits with exact text replacement, including multiple disjoint edits in one call", []string{
		"Use edit for precise changes (edits[].oldText must match exactly)",
		"When changing multiple separate locations in one file, use one edit call with multiple entries in edits[] instead of multiple edit calls",
		"Each edits[].oldText is matched against the original file, not after earlier edits are applied. Do not emit overlapping or nested edits. Merge nearby changes into one edit.",
		"Keep edits[].oldText as small as possible while still being unique in the file. Do not pad with large unchanged regions.",
		"edits[].oldText is matched against the raw file content — never include the line-number prefixes (N | ) shown by read.",
	}
}
