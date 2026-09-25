// Compaction: long sessions are summarized into a checkpoint entry, and
// the LLM context becomes a read-time projection — storage is never
// rewritten (pi's discipline). Only the newest compaction marker
// contributes; older ones are inert position markers.
package session

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"scode/internal/llm"
)

// DefaultCompactionTokens triggers compaction above this context size
// (used when the model's context window is unknown).
const DefaultCompactionTokens = 80_000

// DefaultReserveTokens is the headroom kept below the model's context
// window when the window is known (pi's reserveTokens default).
const DefaultReserveTokens = 16_384

// CompactionEntry marks that everything before it was summarized.
// It serializes with a top-level "kind":"compaction" field, which no
// llm.Message has — the two line types coexist in one JSONL file.
type CompactionEntry struct {
	Kind          string   `json:"kind"` // "compaction"
	Summary       string   `json:"summary"`
	ReadFiles     []string `json:"readFiles,omitempty"`
	ModifiedFiles []string `json:"modifiedFiles,omitempty"`
	CreatedAt     int64    `json:"createdAt"`
}

func NewCompaction(summary string, read, modified []string) CompactionEntry {
	return CompactionEntry{
		Kind:          "compaction",
		Summary:       summary,
		ReadFiles:     read,
		ModifiedFiles: modified,
		CreatedAt:     time.Now().UnixMilli(),
	}
}

// Entry is one storage line: a message or a compaction marker.
type Entry struct {
	Msg        *llm.Message
	Compaction *CompactionEntry
}

func MsgEntry(m llm.Message) Entry { return Entry{Msg: &m} }

// Project computes the LLM context from stored entries: the leading
// system message, the newest compaction rendered as a user message, and
// every message stored after that marker. Without a marker it is the
// full message list. This is the only sanctioned way to derive a
// context from a session (pi's buildSessionProjection, linear case).
func Project(entries []Entry) []llm.Message {
	last := -1
	for i, e := range entries {
		if e.Compaction != nil {
			last = i
		}
	}
	if last == -1 {
		var msgs []llm.Message
		for _, e := range entries {
			if e.Msg != nil {
				msgs = append(msgs, *e.Msg)
			}
		}
		return msgs
	}
	leadingIdx := -1
	for i := 0; i < last; i++ {
		if entries[i].Msg != nil {
			leadingIdx = i
			break
		}
	}
	out := []llm.Message{}
	if leadingIdx >= 0 {
		out = append(out, *entries[leadingIdx].Msg)
	}
	out = append(out, compactionMessage(*entries[last].Compaction))
	for _, e := range entries[last+1:] {
		if e.Msg != nil {
			out = append(out, *e.Msg)
		}
	}
	return out
}

func compactionMessage(c CompactionEntry) llm.Message {
	var b strings.Builder
	b.WriteString("[Earlier conversation was compacted. Summary of the preceding turns:]\n\n")
	b.WriteString(c.Summary)
	if len(c.ReadFiles) > 0 {
		b.WriteString("\n\nFiles read so far: " + strings.Join(c.ReadFiles, ", "))
	}
	if len(c.ModifiedFiles) > 0 {
		b.WriteString("\nFiles modified so far: " + strings.Join(c.ModifiedFiles, ", "))
	}
	return llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.Block{llm.TextBlock(b.String())},
		TS:      c.CreatedAt,
	}
}

// NeedsCompaction estimates the current context size from the most
// recent usage report and compares against the threshold. Non-positive
// thresholds disable compaction.
func NeedsCompaction(entries []Entry, threshold int) bool {
	if threshold <= 0 {
		return false
	}
	msgs := Project(entries)
	for i := len(msgs) - 1; i >= 0; i-- {
		if u := msgs[i].Usage; u != nil {
			total := u.Input + u.CacheRead + u.CacheWrite + u.Output
			return total > int64(threshold)
		}
	}
	return false
}

// FileOps carries the working-set across compaction generations.
type FileOps struct {
	Read     []string
	Modified []string
}

// LatestFileOps recovers the carried-forward working set from the newest
// compaction marker in a record (empty when none exists yet).
func (r *Record) LatestFileOps() FileOps {
	for i := len(r.Entries) - 1; i >= 0; i-- {
		if c := r.Entries[i].Compaction; c != nil {
			return FileOps{Read: c.ReadFiles, Modified: c.ModifiedFiles}
		}
	}
	return FileOps{}
}

// ExtractFileOps walks assistant tool calls since the previous
// compaction (msgs are post-projection) and merges with the carried
// forward sets.
func ExtractFileOps(msgs []llm.Message, prev FileOps) FileOps {
	read := setOf(prev.Read)
	modified := setOf(prev.Modified)
	for _, m := range msgs {
		if m.Role != llm.RoleAssistant {
			continue
		}
		for _, b := range m.Content {
			if b.Kind != llm.BlockToolCall {
				continue
			}
			path := argPath(b.Arguments)
			if path == "" {
				continue
			}
			switch b.Name {
			case "read", "grep", "find":
				read[path] = true
			case "write", "edit":
				modified[path] = true
			}
		}
	}
	return FileOps{Read: sortedKeys(read), Modified: sortedKeys(modified)}
}

func setOf(xs []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func argPath(raw []byte) string {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return ""
	}
	return a.Path
}

// SummarizationSystemPrompt drives the one-off summary call.
const SummarizationSystemPrompt = `You summarize coding-agent conversations for continuation. Produce a dense, factual summary with these sections:
- Task: what the user asked for, and any constraints.
- State: what has been done, what is verified (commands run and their outcomes), what failed.
- Working set: key files, functions, and their relevance.
- Next steps: the immediate actions the next session should take.
Do not include greetings or meta-commentary. Keep it under 400 words.`

// BuildSummaryPrompt renders the conversation for summarization:
// roles and text content, tool calls with arguments, tool results
// head-truncated, whole transcript capped.
func BuildSummaryPrompt(msgs []llm.Message, prev FileOps) string {
	const maxTotal = 100 << 10 // 100 KiB
	var b strings.Builder
	if len(prev.Read) > 0 || len(prev.Modified) > 0 {
		fmt.Fprintf(&b, "Carried forward from earlier compaction:\n")
		if len(prev.Read) > 0 {
			b.WriteString("  Files read: " + strings.Join(prev.Read, ", ") + "\n")
		}
		if len(prev.Modified) > 0 {
			b.WriteString("  Files modified: " + strings.Join(prev.Modified, ", ") + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("Conversation to summarize (most recent last):\n\n")
	for _, m := range msgs {
		if m.Role == llm.RoleSystem {
			continue
		}
		header := string(m.Role)
		switch m.Role {
		case llm.RoleTool:
			header = "tool result"
		case llm.RoleAssistant:
			header = "assistant"
		}
		var parts []string
		for _, blk := range m.Content {
			switch blk.Kind {
			case llm.BlockText:
				parts = append(parts, blk.Text)
			case llm.BlockThinking:
				// Reasoning is reground every turn; not summary material.
			case llm.BlockToolCall:
				parts = append(parts, fmt.Sprintf("[tool call %s %s]", blk.Name, string(blk.Arguments)))
			case llm.BlockToolResult:
				text := blockText(blk.Content)
				if len(text) > 2000 {
					text = text[:2000] + "…"
				}
				label := "tool result"
				if blk.IsError {
					label = "tool error"
				}
				parts = append(parts, fmt.Sprintf("[%s] %s", label, text))
			}
		}
		if len(parts) == 0 {
			continue
		}
		line := header + ": " + strings.Join(parts, "\n")
		if len(line) > 4000 {
			line = line[:4000] + "…"
		}
		b.WriteString(line + "\n\n")
		if b.Len() > maxTotal {
			b.WriteString("(conversation truncated for summarization)\n")
			break
		}
	}
	b.WriteString("Produce the summary now.")
	return b.String()
}

func blockText(blocks []llm.Block) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Kind == llm.BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}
