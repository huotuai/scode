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

	"scode/internal/llm"
)

// DefaultCompactionTokens triggers compaction above this context size
// (used when the model's context window is unknown).
const DefaultCompactionTokens = 80_000

// DefaultReserveTokens is the headroom kept below the model's context
// window when the window is known (pi's reserveTokens default).
const DefaultReserveTokens = 16_384

// DefaultKeepRecentTokens is the size of the recent verbatim tail kept
// across compaction (pi's keepRecentTokens default).
const DefaultKeepRecentTokens = 20_000

// CompactionEntry marks that everything before FirstKeptEntryID was
// summarized (pi's CompactionEntry: file-ops ride in details).
type CompactionEntry struct {
	Summary          string             `json:"summary"`
	FirstKeptEntryID string             `json:"firstKeptEntryId"`
	TokensBefore     int                `json:"tokensBefore"`
	Details          *CompactionDetails `json:"details,omitempty"`

	// FirstKeptIndex is the LEGACY scode index-based kept range. It is
	// only ever read from v1 files and converted to FirstKeptEntryID
	// during migration; new markers leave it nil (and omit it).
	FirstKeptIndex *int `json:"firstKeptIndex,omitempty"`
}

// CompactionDetails carries the working-set across compaction
// generations (pi's CompactionDetails).
type CompactionDetails struct {
	ReadFiles     []string `json:"readFiles,omitempty"`
	ModifiedFiles []string `json:"modifiedFiles,omitempty"`
}

func NewCompaction(summary string, read, modified []string) CompactionEntry {
	c := CompactionEntry{Summary: summary}
	if len(read) > 0 || len(modified) > 0 {
		c.Details = &CompactionDetails{ReadFiles: read, ModifiedFiles: modified}
	}
	return c
}

// Project computes the LLM context from stored entries: the newest
// compaction rendered as a user message, the verbatim kept tail from
// its firstKeptEntryId on (pi's retained range), plus every conversation
// message stored after that marker; without a marker it is the full
// conversation. Session files carry CONVERSATION ONLY — the system
// prompt and tool declarations are rebuilt at load and prepended in
// memory (pi's boundary: storage never contains system messages), so
// system messages found in legacy files are skipped here.
func Project(entries []Entry) []llm.Message {
	idx := projectIndexed(entries)
	out := make([]llm.Message, 0, len(idx))
	for _, im := range idx {
		out = append(out, im.m)
	}
	return out
}

// indexedMsg is a projected message with its source entry index.
type indexedMsg struct {
	m   llm.Message
	src int
}

func projectIndexed(entries []Entry) []indexedMsg {
	last := -1
	for i, e := range entries {
		if e.Compaction != nil {
			last = i
		}
	}
	var out []indexedMsg
	start := 0
	if last >= 0 {
		c := entries[last].Compaction
		out = append(out, indexedMsg{m: compactionMessage(*c), src: last})
		switch {
		case c.FirstKeptEntryID != "":
			// pi's retained range: the verbatim tail from the kept entry
			// to the marker, then post-marker messages.
			start = last + 1
			for i, e := range entries {
				if e.ID == c.FirstKeptEntryID {
					start = i
					break
				}
			}
		case c.FirstKeptIndex != nil:
			// Legacy scode index form (unmigrated in-memory markers).
			start = *c.FirstKeptIndex
			if start < 0 {
				start = 0
			}
			if start > last {
				start = last + 1
			}
		default:
			start = last + 1 // legacy total compaction
		}
	}
	for i := start; i < len(entries); i++ {
		e := entries[i]
		if e.Msg != nil && e.Msg.Role != llm.RoleSystem {
			out = append(out, indexedMsg{m: *e.Msg, src: i})
		}
	}
	return out
}

func compactionMessage(c CompactionEntry) llm.Message {
	var b strings.Builder
	b.WriteString("[Earlier conversation was compacted. Summary of the preceding turns:]\n\n")
	b.WriteString(c.Summary)
	if c.Details != nil {
		if len(c.Details.ReadFiles) > 0 {
			b.WriteString("\n\nFiles read so far: " + strings.Join(c.Details.ReadFiles, ", "))
		}
		if len(c.Details.ModifiedFiles) > 0 {
			b.WriteString("\nFiles modified so far: " + strings.Join(c.Details.ModifiedFiles, ", "))
		}
	}
	return llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.Block{llm.TextBlock(b.String())},
	}
}

// NeedsCompaction estimates the projected context size (pi's
// estimateProjectedContextTokens) and compares against the threshold.
// Non-positive thresholds disable compaction.
func NeedsCompaction(entries []Entry, threshold int) bool {
	if threshold <= 0 {
		return false
	}
	return EstimateProjectedTokens(entries) > int64(threshold)
}

// EstimateProjectedTokens mirrors pi's estimateProjectedContextTokens:
// usage recorded before the newest compaction describes the
// PRE-compaction context and is distrusted — only usages from entries
// after the newest marker count, plus a chars/4 estimate of the
// messages trailing them; without any trustworthy usage, a pure
// estimate of the whole projection.
func EstimateProjectedTokens(entries []Entry) int64 {
	last := -1
	for i, e := range entries {
		if e.Compaction != nil {
			last = i
		}
	}
	msgs := projectIndexed(entries)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].src <= last {
			continue // pre-marker (kept tail): stale usage
		}
		if u := ValidUsage(msgs[i].m); u != nil {
			total := u.TotalTokens()
			for j := i + 1; j < len(msgs); j++ {
				total += int64(EstimateTokens(msgs[j].m))
			}
			return total
		}
	}
	var total int64
	for _, im := range msgs {
		total += int64(EstimateTokens(im.m))
	}
	return total
}

// estimatedImageChars stands in for an image block (pi's
// ESTIMATED_IMAGE_CHARS): 4800 chars 248 1200 tokens.
const estimatedImageChars = 4800

// EstimateTokens approximates one message's tokens with the chars/4
// heuristic (pi's estimateTokens; a conservative overestimate).
func EstimateTokens(m llm.Message) int {
	chars := 0
	for _, b := range m.Content {
		switch b.Kind {
		case llm.BlockText, llm.BlockThinking:
			chars += len(b.Text)
		case llm.BlockToolCall:
			chars += len(b.Name) + len(b.Arguments)
		case llm.BlockToolResult:
			for _, nb := range b.Content {
				chars += len(nb.Text) + len(nb.Data)
			}
		case llm.BlockImage:
			chars += estimatedImageChars
		}
	}
	return (chars + 3) / 4
}

// ValidUsage mirrors pi's getAssistantUsage: aborted, error, and
// all-zero usages carry no trustworthy accounting.
func ValidUsage(m llm.Message) *llm.Usage {
	if m.Role != llm.RoleAssistant || m.Usage == nil {
		return nil
	}
	if m.StopReason == llm.StopError || m.StopReason == llm.StopAborted {
		return nil
	}
	if m.Usage.TotalTokens() <= 0 {
		return nil
	}
	return m.Usage
}

// EstimateContextTokens mirrors pi's estimateContextTokens: the last
// valid assistant usage plus a chars/4 estimate of every message after
// it; without any usage, the chars/4 sum of all messages.
func EstimateContextTokens(msgs []llm.Message) int64 {
	usageIdx := -1
	var base int64
	for i := len(msgs) - 1; i >= 0; i-- {
		if u := ValidUsage(msgs[i]); u != nil {
			usageIdx, base = i, u.TotalTokens()
			break
		}
	}
	var total int64
	if usageIdx >= 0 {
		total = base
	}
	for i := usageIdx + 1; i < len(msgs); i++ {
		total += int64(EstimateTokens(msgs[i]))
	}
	return total
}

// FindCutPoint returns the entry index where the verbatim kept tail
// starts (pi's findCutPoint): walk backwards accumulating estimated
// tokens until keepRecentTokens is covered, then cut at the nearest
// entry at or after that point which is not a tool-result message (a
// cut must never separate an assistant's tool calls from their
// results). Returns 0 when the whole history fits (nothing worth
// summarizing); returns len(entries) with keepRecentTokens <= 0 (keep
// nothing — total compaction).
func FindCutPoint(entries []Entry, keepRecentTokens int) int {
	if keepRecentTokens <= 0 {
		return len(entries)
	}
	var acc int64
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Msg == nil {
			continue
		}
		t := EstimateTokens(*e.Msg)
		if t == 0 {
			continue
		}
		acc += int64(t)
		if acc < int64(keepRecentTokens) {
			continue
		}
		// Nearest valid cut at or after i.
		for j := i; j < len(entries); j++ {
			if ej := entries[j]; ej.Msg != nil && ej.Msg.Role != llm.RoleTool {
				return j
			}
		}
		// Trailing tool results only: keep their assistant call instead.
		for j := i - 1; j >= 0; j-- {
			if ej := entries[j]; ej.Msg != nil && ej.Msg.Role != llm.RoleTool {
				return j
			}
		}
		return 0
	}
	return 0 // everything fits in the kept tail
}

// FileOps carries the working-set across compaction generations.
type FileOps struct {
	Read     []string
	Modified []string
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

// SummarizationSystemPrompt drives the one-off summary call (pi's).
const SummarizationSystemPrompt = `You are a context summarization assistant. Your task is to read a conversation between a user and an AI assistant, then produce a structured summary following the exact format specified.

Do NOT continue the conversation. Do NOT respond to any questions in the conversation. ONLY output the structured summary.`

// SummarizationPrompt is appended after the serialized conversation on
// a first compaction (pi's structured checkpoint format).
const SummarizationPrompt = `The messages above are a conversation to summarize. Create a structured context checkpoint summary that another LLM will use to continue the work.

Use this EXACT format:

## Goal
[What is the user trying to accomplish? Can be multiple items if the session covers different tasks.]

## Constraints & Preferences
- [Any constraints, preferences, or requirements mentioned by user]
- [Or "(none)" if none were mentioned]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Current work]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered list of what should happen next]

## Critical Context
- [Any data, examples, or references needed to continue]
- [Or "(none)" if not applicable]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

// UpdateSummarizationPrompt drives the second and later compactions:
// merge the new messages into the previous summary instead of
// re-summarizing from scratch (pi's update flow).
const UpdateSummarizationPrompt = `The messages above are NEW conversation messages to incorporate into the existing summary provided in <previous-summary> tags.

Update the existing structured summary with new information. RULES:
- PRESERVE all existing information from the previous summary
- ADD new progress, decisions, and context from the new messages
- UPDATE the Progress section: move items from "In Progress" to "Done" when completed
- UPDATE "Next Steps" based on what was accomplished
- PRESERVE exact file paths, function names, and error messages
- If something is no longer relevant, you may remove it

Use this EXACT format:

## Goal
[Preserve existing goals, add new ones if the task expanded]

## Constraints & Preferences
- [Preserve existing, add new ones discovered]

## Progress
### Done
- [x] [Include previously done items AND newly completed items]

### In Progress
- [ ] [Current work - update based on progress]

### Blocked
- [Current blockers - remove if resolved]

## Key Decisions
- **[Decision]**: [Brief rationale] (preserve all previous, add new)

## Next Steps
1. [Update based on current state]

## Critical Context
- [Preserve important context, add new if needed]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

// TurnPrefixSummarizationPrompt summarizes the partial turn preceding
// a mid-turn cut point (pi's TURN_PREFIX_SUMMARIZATION_PROMPT).
const TurnPrefixSummarizationPrompt = `The messages above are earlier context from an ongoing conversation. Later messages are stored separately and do not need to be reconstructed.

Create a concise checkpoint of the user's request and the progress shown above. This checkpoint will be placed before the later messages so the conversation can continue with the necessary context.

## Original Request
[What did the user ask for?]

## Progress So Far
- [Key decisions and work completed in these messages]

## Context Needed to Continue
- [Information from these messages needed to understand the later work]

Only summarize information explicitly present above. Do not infer or recreate later messages.`

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
