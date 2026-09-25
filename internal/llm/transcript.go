package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// Transcript is an append-only conversation. It is the unit of persistence
// (sessions serialize transcripts as JSONL) and the source from which every
// provider request is derived. Appended messages are frozen: the zero-length
// copy returned by Messages protects callers from mutation, and nothing in
// this package ever edits an existing entry. Prefix byte-stability is the
// load-bearing property — see the package comment.
type Transcript struct {
	messages []Message
}

// NewTranscript builds a transcript from a leading system message plus any
// number of follow-up messages, validating the shape once at construction.
// The leading message declares the base prompt, initial sections, and
// initial tool set; it cannot remove tools (there is nothing prior).
func NewTranscript(leading Message, rest ...Message) (*Transcript, error) {
	if leading.Role != RoleSystem {
		return nil, fmt.Errorf("leading message must be system, got %q", leading.Role)
	}
	if len(leading.ToolsRemoved) > 0 {
		return nil, fmt.Errorf("leading system message cannot remove tools")
	}
	t := &Transcript{}
	if err := t.Append(leading); err != nil {
		return nil, err
	}
	for i := range rest {
		if err := t.Append(rest[i]); err != nil {
			return nil, fmt.Errorf("rest[%d]: %w", i, err)
		}
	}
	return t, nil
}

// Append validates and freezes a message onto the end of the transcript.
// The first message must be the declaring system message; later system
// messages carry deltas only (tool changes, section patches, prompt
// appends) so the leading declaration is never rewritten. Everything here
// is append-only by construction: growth never touches prior bytes.
func (t *Transcript) Append(m Message) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if len(t.messages) == 0 {
		if m.Role != RoleSystem {
			return fmt.Errorf("transcript must start with a system message, got %q", m.Role)
		}
		if m.TS != 0 {
			return fmt.Errorf("leading system message must have ts=0, got %d", m.TS)
		}
		if len(m.ToolsRemoved) > 0 {
			return fmt.Errorf("leading system message cannot remove tools")
		}
	}
	t.messages = append(t.messages, m)
	return nil
}

// AppendNow stamps m with the current unix-millisecond time and appends it.
// System messages stay unstamped (delta position is their identity).
func (t *Transcript) AppendNow(m Message) error {
	if m.Role != RoleSystem {
		m.TS = time.Now().UnixMilli()
	}
	return t.Append(m)
}

// Messages returns a defensive copy of the transcript so callers cannot
// violate append-only-ness through the returned slice.
func (t *Transcript) Messages() []Message {
	out := make([]Message, len(t.messages))
	copy(out, t.messages)
	return out
}

// Len returns the number of messages.
func (t *Transcript) Len() int { return len(t.messages) }

// CanonicalBytes serializes the first n messages deterministically. Struct
// fields marshal in declaration order and there are no maps on the message
// path, so equal transcripts always produce equal bytes. This is the basis
// of the prefix-stability tests and can also back content hashing.
func CanonicalBytes(msgs []Message) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i := range msgs {
		if err := enc.Encode(msgs[i]); err != nil { // one JSON object per line
			return nil, fmt.Errorf("message %d: %w", i, err)
		}
	}
	return buf.Bytes(), nil
}

// Context is a not-yet-normalized request: a prompt, tool set, and history.
type Context struct {
	SystemPrompt string
	Tools        []Tool
	Messages     []Message
}

// NormalizeContext folds c into a TranscriptContext: the system prompt and
// tool declarations become the leading system message (ts=0) when either is
// present. This is the only sanctioned way to produce a TranscriptContext,
// mirroring pi's contract that every provider-facing function receives the
// normalized shape.
func NormalizeContext(c Context) (*Transcript, error) {
	if len(c.Messages) > 0 && c.Messages[0].Role == RoleSystem {
		// History already carries its own leading declaration.
		t := &Transcript{}
		for i := range c.Messages {
			if err := t.Append(c.Messages[i]); err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
		}
		if len(c.Tools) > 0 || c.SystemPrompt != "" {
			return nil, fmt.Errorf("context has both a leading system message and prompt/tools")
		}
		return t, nil
	}
	leading := Message{
		Role:    RoleSystem,
		Content: []Block{TextBlock(c.SystemPrompt)},
	}
	if len(c.Tools) > 0 {
		leading.ToolsAdded = append([]Tool(nil), c.Tools...)
	}
	t := &Transcript{}
	if err := t.Append(leading); err != nil {
		return nil, err
	}
	for i := range c.Messages {
		if err := t.Append(c.Messages[i]); err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
	}
	return t, nil
}

// CurrentTools replays every system message's tool deltas in order and
// returns the tool set in force at the end of the transcript, in
// first-declaration order (pi's getCurrentTools). Ordering follows map
// insertion semantics: a removal drops the slot, a re-add appends, and a
// plain redeclaration keeps its original position.
func CurrentTools(msgs []Message) []Tool {
	tools := make(map[string]Tool)
	var order []string
	for i := range msgs {
		if msgs[i].Role != RoleSystem {
			continue
		}
		for _, n := range msgs[i].ToolsRemoved {
			delete(tools, n)
			order = removeString(order, n)
		}
		for _, t := range msgs[i].ToolsAdded {
			if _, ok := tools[t.Name]; !ok {
				order = append(order, t.Name)
			}
			tools[t.Name] = t
		}
	}
	out := make([]Tool, 0, len(order))
	for _, n := range order {
		out = append(out, tools[n])
	}
	return out
}

func removeString(xs []string, v string) []string {
	for i, x := range xs {
		if x == v {
			return append(xs[:i:i], xs[i+1:]...)
		}
	}
	return xs
}

// CurrentSystemMessage replays every system message into the single
// effective leading system message: contents concatenate in order, sections
// patch by name (later wins, Delete removes), and the tool set is resolved
// by CurrentTools. The result is what a provider without mid-conversation
// system support sends as its top-level prompt (pi's getCurrentSystemMessage).
func CurrentSystemMessage(msgs []Message) *Message {
	var contents []string
	type secEntry struct {
		value  string
		delete bool
	}
	sections := make(map[string]secEntry)
	var order []string
	var ts int64
	hasSystem := false
	for i := range msgs {
		if msgs[i].Role != RoleSystem {
			continue
		}
		hasSystem = true
		if ts == 0 {
			ts = msgs[i].TS
		}
		if s := messageText(msgs[i]); s != "" {
			contents = append(contents, s)
		}
		for _, sec := range msgs[i].Sections {
			if _, ok := sections[sec.Name]; !ok {
				order = append(order, sec.Name)
			}
			sections[sec.Name] = secEntry{value: sec.Value, delete: sec.Delete}
		}
	}
	if !hasSystem {
		return nil
	}
	out := &Message{Role: RoleSystem, TS: ts}
	if len(contents) > 0 {
		out.Content = []Block{TextBlock(joinNonEmpty(contents, "\n\n"))}
	}
	var secs []Section
	for _, name := range order {
		e := sections[name]
		if e.delete {
			continue
		}
		secs = append(secs, Section{Name: name, Value: e.value})
	}
	out.Sections = secs
	if tools := CurrentTools(msgs); len(tools) > 0 {
		out.ToolsAdded = tools
	}
	return out
}

func messageText(m Message) string {
	var s string
	for _, b := range m.Content {
		if b.Kind == BlockText {
			if s != "" {
				s += "\n"
			}
			s += b.Text
		}
	}
	return s
}

func joinNonEmpty(xs []string, sep string) string {
	var out string
	for _, x := range xs {
		if x == "" {
			continue
		}
		if out != "" {
			out += sep
		}
		out += x
	}
	return out
}

// DeclarationBytes returns the canonical serialization of a tool declaration
// for exact comparison. Byte-level (not semantic) equality is intended: it
// matches how providers cache the tools array.
func DeclarationBytes(t Tool) ([]byte, error) {
	return json.Marshal(struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	}{Name: t.Name, Description: t.Description, Parameters: compactJSON(t.Parameters)})
}

// DeclarationsEqual reports whether two tools declare the same interface to
// the model (pi's declarationsEqual).
func DeclarationsEqual(a, b Tool) bool {
	ab, err := DeclarationBytes(a)
	if err != nil {
		return false
	}
	bb, err := DeclarationBytes(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

// ToolStateChanges describes how to get from prev to cur. A changed
// definition is expressed as removal followed by addition, which
// transports that reference tools by name can replay (pi's
// getToolStateChanges).
type ToolStateChanges struct {
	ToolsAdded   []Tool
	ToolsRemoved []string
}

// DiffTools compares two complete tool states.
func DiffTools(prev, cur []Tool) ToolStateChanges {
	prevMap := make(map[string]Tool, len(prev))
	for _, t := range prev {
		prevMap[t.Name] = t
	}
	curMap := make(map[string]Tool, len(cur))
	for _, t := range cur {
		curMap[t.Name] = t
	}
	var out ToolStateChanges
	for _, t := range cur {
		if p, ok := prevMap[t.Name]; !ok || !DeclarationsEqual(p, t) {
			out.ToolsAdded = append(out.ToolsAdded, t)
		}
	}
	for _, t := range prev {
		if _, ok := curMap[t.Name]; !ok {
			out.ToolsRemoved = append(out.ToolsRemoved, t.Name)
		}
	}
	return out
}

// compactJSON re-serializes raw JSON with sorted map keys so equivalent
// documents compare equal regardless of source formatting.
func compactJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return json.RawMessage(bytes.TrimSpace(raw)) // not valid JSON: keep bytes
	}
	out, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(bytes.TrimSpace(raw))
	}
	return out
}
