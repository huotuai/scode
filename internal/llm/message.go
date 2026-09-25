// Package llm defines scode's provider-neutral LLM message model.
//
// The model is designed around one non-negotiable property, borrowed from pi:
// the serialized transcript prefix must be byte-stable as a conversation grows.
// Provider-side prompt caches (Anthropic cache_control, OpenAI automatic prefix
// caching) only hit when every byte before the newest message is identical
// between requests. The invariants that guarantee this are enforced by the
// Transcript type and its tests:
//
//  1. Transcripts are append-only. Existing messages are never mutated.
//  2. The system prompt and the initial tool declarations live in the leading
//     system message, whose timestamp is fixed at 0 (non-volatile).
//  3. Tool changes mid-session are expressed as *delta* system messages
//     carrying ToolsAdded/ToolsRemoved, never by rewriting the leading
//     declaration. Prompt-section changes likewise arrive as Section patches.
//  4. Volatile content (dates, git status, model info) must never enter the
//     prompt; it reaches the model through shell-tool environment variables.
//
// System prompt sections replay deterministically: later Section patches with
// the same name replace earlier ones, and Sections preserves append order,
// so JSON marshaling of a replayed prompt is reproducible.
package llm

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Role is the agent-level role of a message. Providers map these onto their
// own wire roles at the request boundary (e.g. toolResult blocks are folded
// into user messages for the Anthropic Messages API).
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "toolResult"
)

// StopReason describes why an assistant turn ended.
type StopReason string

const (
	StopEndTurn StopReason = "stop"    // model finished its reply
	StopLength  StopReason = "length"  // ran out of output tokens
	StopToolUse StopReason = "toolUse" // model requested tool calls
	StopAborted StopReason = "aborted" // caller cancelled via context
	StopError   StopReason = "error"   // provider or transport failure
)

// BlockKind discriminates content blocks.
type BlockKind string

const (
	BlockText       BlockKind = "text"
	BlockThinking   BlockKind = "thinking"
	BlockToolCall   BlockKind = "toolCall"
	BlockToolResult BlockKind = "toolResult"
	BlockImage      BlockKind = "image"
)

// Block is one content block of a message. A single flat struct with a Kind
// discriminator keeps JSON marshaling deterministic (struct field order is
// fixed by declaration), which the prefix-stability guarantee relies on.
type Block struct {
	Kind BlockKind `json:"kind"`

	// Text carries the payload for BlockText and BlockThinking.
	Text string `json:"text,omitempty"`
	// Signature authenticates a thinking block across provider round-trips.
	Signature string `json:"signature,omitempty"`

	// ID identifies a tool call; toolResult blocks reference the same ID.
	ID string `json:"id,omitempty"`
	// Name is the tool name on a toolCall block.
	Name string `json:"name,omitempty"`
	// Arguments holds the tool call parameters as raw JSON.
	Arguments json.RawMessage `json:"arguments,omitempty"`

	// Content holds nested blocks of a toolResult (text and image blocks).
	Content []Block `json:"content,omitempty"`
	// IsError marks a toolResult as a failure the model should react to.
	IsError bool `json:"isError,omitempty"`

	// MimeType and Data (base64) carry image blocks.
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
}

func TextBlock(s string) Block     { return Block{Kind: BlockText, Text: s} }
func ThinkingBlock(s string) Block { return Block{Kind: BlockThinking, Text: s} }
func ToolCallBlock(id, name string) Block {
	return Block{Kind: BlockToolCall, ID: id, Name: name}
}

// Section is one named slice of the system prompt (pi's section model).
// Replaying a transcript, a later Section with the same Name replaces an
// earlier one; Delete removes it. Sections live only on system messages.
type Section struct {
	Name   string `json:"name"`
	Value  string `json:"value,omitempty"`
	Delete bool   `json:"delete,omitempty"`
}

// Tool is a tool declaration as seen by the model. Parameters is a JSON Schema
// document kept as raw JSON: byte-exact comparison of declarations mirrors
// pi's declarationsEqual (key order and whitespace are significant).
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// Usage reports token accounting for an assistant message, split by cache
// state from day one — cache behavior must be observable to be tunable.
type Usage struct {
	Input        int64 `json:"input,omitempty"`
	Output       int64 `json:"output,omitempty"`
	CacheRead    int64 `json:"cacheRead,omitempty"`
	CacheWrite   int64 `json:"cacheWrite,omitempty"`
	CacheWrite1h int64 `json:"cacheWrite1h,omitempty"`
}

// Add returns the element-wise sum of two usages (aggregate accounting).
func (u Usage) Add(o Usage) Usage {
	return Usage{
		Input:        u.Input + o.Input,
		Output:       u.Output + o.Output,
		CacheRead:    u.CacheRead + o.CacheRead,
		CacheWrite:   u.CacheWrite + o.CacheWrite,
		CacheWrite1h: u.CacheWrite1h + o.CacheWrite1h,
	}
}

// TotalTokens returns the sum of all token buckets (cost-relevant volume).
func (u Usage) TotalTokens() int64 {
	return u.Input + u.Output + u.CacheRead + u.CacheWrite + u.CacheWrite1h
}

// Message is the atom of a transcript.
type Message struct {
	Role    Role    `json:"role"`
	Content []Block `json:"content,omitempty"`
	// TS is the message timestamp in unix milliseconds. The leading system
	// message is fixed at 0 so the prefix never varies with wall-clock time.
	TS int64 `json:"ts,omitempty"`

	// System-message extras: tool-state deltas and prompt-section patches.
	ToolsAdded   []Tool    `json:"toolsAdded,omitempty"`
	ToolsRemoved []string  `json:"toolsRemoved,omitempty"`
	Sections     []Section `json:"sections,omitempty"`

	// Assistant-message extras.
	StopReason StopReason `json:"stopReason,omitempty"`
	Usage      *Usage     `json:"usage,omitempty"`
	Model      string     `json:"model,omitempty"`
	Provider   string     `json:"provider,omitempty"`
	// Error carries the failure text when StopReason is StopError.
	Error string `json:"error,omitempty"`
}

// Validate checks per-role invariants a caller must uphold before appending.
func (m *Message) Validate() error {
	switch m.Role {
	case RoleSystem:
		for _, t := range m.ToolsAdded {
			if t.Name == "" {
				return errors.New("system message: added tool with empty name")
			}
		}
		for _, n := range m.ToolsRemoved {
			if n == "" {
				return errors.New("system message: removed tool with empty name")
			}
		}
		for _, s := range m.Sections {
			if s.Name == "" {
				return errors.New("system message: section with empty name")
			}
		}
	case RoleUser, RoleTool:
		if len(m.Content) == 0 {
			return fmt.Errorf("%s message: empty content", m.Role)
		}
	case RoleAssistant:
		if len(m.Content) == 0 && m.Error == "" {
			return errors.New("assistant message: empty content and no error")
		}
		if m.StopReason == StopError && m.Error == "" {
			return errors.New("assistant message: stopReason error requires error text")
		}
	default:
		return fmt.Errorf("unknown role %q", m.Role)
	}
	return nil
}
