package cli

// The memory subsystem's host side: the system-prompt section
// (injected at Setup when autoMemory is on), the extractor (a
// SideChannel call folding the conversation into memory operations),
// the close-time auto-extraction (memoryAutoExtraction on), and the
// /memory listing surfaces.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"scode/internal/llm"
	"scode/internal/memory"
)

// extractSystemPrompt steers the extraction call. The reply contract
// is a bare JSON operations array.
const extractSystemPrompt = `You maintain a coding assistant's long-term memory for one project. You receive the CURRENT memories (JSON) and a DIGEST of the recent conversation. Reply with ONLY a JSON array of operations — no prose, no markdown fences:
[{"op":"add","category":"project|preference|fact|task","content":"..."},
 {"op":"update","id":"<existing id>","content":"..."},
 {"op":"delete","id":"<existing id>"}]
Rules: remember only durable, reusable facts (build commands, conventions, user preferences, constraints, ongoing work); forget chatter and one-off detail. Merge with existing memories — update instead of duplicating, delete entries the conversation contradicts. Keep the total under 40; one line per memory, in the conversation's language. Return [] when nothing is worth remembering.`

// memorySection builds the injected section value ("" = no section).
//
// It runs ONCE at Setup and the section is never patched mid-session
// (toggling picks in /memory or running /memory extract takes effect
// on the NEXT launch). That is a deliberate cache trade, not an
// oversight: the section lives inside the system prompt at the head
// of every request, so a mid-session rewrite would invalidate the
// whole prefix cache — a few minutes of stale memory is the cheaper
// price.
func (a *App) memorySection() string {
	s := a.Settings
	if s == nil || !s.AutoMemoryOn() {
		return ""
	}
	entries, err := a.memStore.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "memory: %v\n", err)
		return ""
	}
	return memory.FormatForPrompt(entries, s.TypedMemoryOn(), s.MemoryRelevanceOn())
}

// MemoryExtract runs one extraction pass NOW (the manual trigger; the
// close-time path calls the same core). Returns a human summary.
func (a *App) MemoryExtract(ctx context.Context) (string, error) {
	if a.Settings == nil || !a.Settings.AutoMemoryOn() {
		return "", fmt.Errorf("Auto Memory 已关闭 (/config)")
	}
	digest := a.memoryDigest()
	if digest == "" {
		return "(会话没有可提取的对话)", nil
	}
	entries, err := a.memStore.Load()
	if err != nil {
		return "", err
	}
	cur, _ := json.Marshal(entries)
	prompt := "CURRENT MEMORIES:\n" + string(cur) + "\n\nDIGEST:\n" + digest
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := a.Agent.SideChannel(ctx, extractSystemPrompt, prompt)
	if err != nil {
		return "", err
	}
	added, updated, deleted := a.applyMemoryOps(out, a.Sess.Header().ID)
	return fmt.Sprintf("记忆提取完成: 新增 %d · 更新 %d · 删除 %d", added, updated, deleted), nil
}

// applyMemoryOps parses the model's operation array (fences and stray
// prose tolerated) and commits it.
func (a *App) applyMemoryOps(out, source string) (added, updated, deleted int) {
	ops := parseMemoryOps(out)
	if len(ops) == 0 {
		return 0, 0, 0
	}
	return a.memStore.Apply(ops, source)
}

// parseMemoryOps extracts the JSON array from a model reply.
func parseMemoryOps(out string) []memory.Op {
	s := strings.TrimSpace(out)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	lo := strings.IndexByte(s, '[')
	hi := strings.LastIndexByte(s, ']')
	if lo < 0 || hi <= lo {
		return nil
	}
	var ops []memory.Op
	if err := json.Unmarshal([]byte(s[lo:hi+1]), &ops); err != nil {
		fmt.Fprintf(os.Stderr, "memory: unparseable extraction reply: %v\n", err)
		return nil
	}
	return ops
}

// memoryDigest renders the recent conversation for extraction: the
// last conversation messages' text, capped.
func (a *App) memoryDigest() string {
	var lines []string
	total := 0
	msgs := a.Tr.Messages()
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != llm.RoleUser && m.Role != llm.RoleAssistant {
			continue
		}
		var txt string
		for _, b := range m.Content {
			if b.Kind == llm.BlockText {
				txt += b.Text
			}
		}
		txt = strings.TrimSpace(txt)
		if txt == "" {
			continue
		}
		if r := []rune(txt); len(r) > 2000 {
			txt = string(r[:2000]) + "…"
		}
		line := string(m.Role) + ": " + strings.ReplaceAll(txt, "\n", " ")
		lines = append([]string{line}, lines...)
		total += len(line)
		if total > 24_000 || len(lines) > 60 {
			break
		}
	}
	return strings.Join(lines, "\n")
}

// maybeAutoExtractMemories is the close-time path: extraction runs when
// autoMemory AND memoryAutoExtraction are both on and the session had a
// real exchange. Best-effort — failures print to stderr, never block
// the exit with an error.
func (a *App) maybeAutoExtractMemories() {
	if a.memStore == nil || a.Settings == nil {
		return
	}
	if !a.Settings.AutoMemoryOn() || !a.Settings.MemoryAutoExtractOn() || !a.interactive {
		return
	}
	if a.memoryDigest() == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	summary, err := a.MemoryExtract(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memory extraction: %v\n", err)
		return
	}
	fmt.Fprintln(os.Stderr, "memory: "+summary)
}

// MemoryEntries returns the stored entries for the /memory manager.
func (a *App) MemoryEntries() ([]memory.Entry, error) {
	entries, err := a.memStore.Load()
	if err != nil {
		return nil, err
	}
	memory.SortEntries(entries)
	return entries, nil
}

// MemoryToggleSelected flips one entry's relevance-selection pick.
func (a *App) MemoryToggleSelected(id string) (bool, error) {
	return a.memStore.ToggleSelected(id)
}

// MemoryDelete removes one entry.
func (a *App) MemoryDelete(id string) error {
	return a.memStore.Delete(id)
}

// MemoryListText renders the /memory listing (REPL surface).
func (a *App) MemoryListText() string {
	entries, err := a.memStore.Load()
	if err != nil {
		return "error: " + err.Error()
	}
	if len(entries) == 0 {
		return "暂无记忆 — 开启 Memory Auto Extraction (/config) 会在会话结束时自动提取,或 /memory extract 手动触发"
	}
	memory.SortEntries(entries)
	rows := make([]string, 0, len(entries)+1)
	for _, e := range entries {
		row := "  [" + memory.CategoryLabel(e.Category) + "] " + e.Content
		if e.Selected {
			row += " · 已选"
		}
		rows = append(rows, row)
	}
	return fmt.Sprintf("记忆 (%d):\n%s\n开启 Memory Relevance 后仅注入“已选”条目 · TUI 里 /memory 管理", len(entries), strings.Join(rows, "\n"))
}
