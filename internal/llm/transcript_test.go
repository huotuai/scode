package llm

import (
	"bytes"
	"encoding/json"
	"testing"
)

func mustTranscript(t *testing.T, leading Message, rest ...Message) *Transcript {
	t.Helper()
	tr, err := NewTranscript(leading, rest...)
	if err != nil {
		t.Fatalf("NewTranscript: %v", err)
	}
	return tr
}

func leadingSystem(prompt string, tools ...Tool) Message {
	m := Message{Role: RoleSystem, Content: []Block{TextBlock(prompt)}}
	if len(tools) > 0 {
		m.ToolsAdded = tools
	}
	return m
}

func userMsg(text string) Message {
	return Message{Role: RoleUser, Content: []Block{TextBlock(text)}, TS: 1}
}

func assistantMsg(text string) Message {
	return Message{Role: RoleAssistant, Content: []Block{TextBlock(text)}, StopReason: StopEndTurn, TS: 2, Usage: &Usage{Input: 10, Output: 5}}
}

// snapshot returns the canonical serialization of the first n messages.
func snapshot(t *testing.T, tr *Transcript, n int) []byte {
	t.Helper()
	msgs := tr.Messages()
	if n > len(msgs) {
		t.Fatalf("snapshot: n=%d > len=%d", n, len(msgs))
	}
	b, err := CanonicalBytes(msgs[:n])
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	return b
}

func assertPrefixStable(t *testing.T, tr *Transcript, prefixLen int, grow func(tr *Transcript)) {
	t.Helper()
	before := snapshot(t, tr, prefixLen)
	grow(tr)
	after := snapshot(t, tr, prefixLen)
	if !bytes.Equal(before, after) {
		t.Fatalf("prefix mutated after growth:\nbefore: %s\nafter:  %s", before, after)
	}
}

// The three load-bearing scenarios from the plan: appending messages,
// tool-state deltas, and prompt-section patches must all leave the
// serialized prefix byte-identical.
func TestPrefixStabilityAppendMessages(t *testing.T) {
	tr := mustTranscript(t, leadingSystem("You are scode."),
		userMsg("hello"),
		assistantMsg("hi"))
	assertPrefixStable(t, tr, 3, func(tr *Transcript) {
		if err := tr.Append(Message{Role: RoleUser, Content: []Block{TextBlock("do a thing")}, TS: 3}); err != nil {
			t.Fatal(err)
		}
		if err := tr.Append(Message{
			Role:       RoleAssistant,
			Content:    []Block{ToolCallBlock("t1", "bash")},
			StopReason: StopToolUse, TS: 4,
		}); err != nil {
			t.Fatal(err)
		}
		if err := tr.Append(Message{
			Role:    RoleTool,
			Content: []Block{{Kind: BlockToolResult, ID: "t1", Content: []Block{TextBlock("done")}}},
			TS:      5,
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPrefixStabilityToolDelta(t *testing.T) {
	tools := []Tool{{Name: "read", Description: "read a file"}, {Name: "bash", Description: "run a command"}}
	tr := mustTranscript(t, leadingSystem("You are scode.", tools...),
		userMsg("hello"))
	assertPrefixStable(t, tr, 2, func(tr *Transcript) {
		// Session grows: enable edit+write, disable nothing.
		delta := Message{Role: RoleSystem, ToolsAdded: []Tool{
			{Name: "edit", Description: "edit a file"},
			{Name: "write", Description: "write a file"},
		}}
		if err := tr.Append(delta); err != nil {
			t.Fatal(err)
		}
	})
	got := CurrentTools(tr.Messages())
	want := []string{"read", "bash", "edit", "write"}
	if len(got) != len(want) {
		t.Fatalf("CurrentTools len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Name != want[i] {
			t.Fatalf("CurrentTools[%d] = %s, want %s", i, got[i].Name, want[i])
		}
	}
}

func TestPrefixStabilitySectionPatch(t *testing.T) {
	tr := mustTranscript(t, func() Message {
		m := leadingSystem("You are scode.")
		m.Sections = []Section{{Name: "rules", Value: "be terse"}}
		return m
	}(),
		userMsg("hello"))
	assertPrefixStable(t, tr, 2, func(tr *Transcript) {
		patch := Message{Role: RoleSystem, Sections: []Section{{Name: "rules", Value: "be terse, cite files"}}}
		if err := tr.Append(patch); err != nil {
			t.Fatal(err)
		}
	})
	sys := CurrentSystemMessage(tr.Messages())
	if sys == nil {
		t.Fatal("CurrentSystemMessage returned nil")
	}
	for _, s := range sys.Sections {
		if s.Name == "rules" && s.Value != "be terse, cite files" {
			t.Fatalf("rules section = %q, want patched value", s.Value)
		}
	}
}

func TestCurrentToolsOrdering(t *testing.T) {
	msgs := []Message{
		leadingSystem("p", Tool{Name: "a"}, Tool{Name: "b"}, Tool{Name: "c"}),
		{Role: RoleSystem, ToolsRemoved: []string{"b"}},
		{Role: RoleSystem, ToolsAdded: []Tool{{Name: "b", Description: "v2"}}},      // re-add moves to end
		{Role: RoleSystem, ToolsAdded: []Tool{{Name: "a", Description: "changed"}}}, // redeclare keeps position
	}
	got := CurrentTools(msgs)
	want := []string{"a", "c", "b"}
	if len(got) != len(want) {
		t.Fatalf("got %d tools, want %d: %v", len(got), len(want), toolNames(got))
	}
	for i := range want {
		if got[i].Name != want[i] {
			t.Fatalf("order = %v, want %v", toolNames(got), want)
		}
	}
	if got[0].Description != "changed" {
		t.Fatalf("redeclared a should have new definition, got %q", got[0].Description)
	}
}

func toolNames(ts []Tool) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

func TestCurrentSystemMessageReplay(t *testing.T) {
	msgs := []Message{
		func() Message {
			m := leadingSystem("base prompt")
			m.Sections = []Section{{Name: "cwd", Value: "/old"}, {Name: "rules", Value: "v1"}, {Name: "temp", Value: "x"}}
			return m
		}(),
		{Role: RoleSystem, Sections: []Section{
			{Name: "rules", Value: "v2"},
			{Name: "temp", Delete: true},
		}},
		{Role: RoleSystem, Content: []Block{TextBlock("appended context")}},
	}
	sys := CurrentSystemMessage(msgs)
	if got := messageText(*sys); got != "base prompt\n\nappended context" {
		t.Fatalf("prompt text = %q", got)
	}
	secs := map[string]string{}
	for _, s := range sys.Sections {
		secs[s.Name] = s.Value
	}
	if secs["rules"] != "v2" || secs["cwd"] != "/old" {
		t.Fatalf("sections replayed wrong: %v", secs)
	}
	if _, ok := secs["temp"]; ok {
		t.Fatal("deleted section survived replay")
	}
}

func TestDeclarationsEqual(t *testing.T) {
	schemaA := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
	schemaAReordered := json.RawMessage(`{ "properties": {"path": {"type": "string"}}, "type": "object" }`)
	a := Tool{Name: "read", Description: "d", Parameters: schemaA}
	if !DeclarationsEqual(a, Tool{Name: "read", Description: "d", Parameters: schemaAReordered}) {
		t.Fatal("semantically identical schemas must compare equal")
	}
	if DeclarationsEqual(a, Tool{Name: "read", Description: "d2", Parameters: schemaA}) {
		t.Fatal("description change must compare unequal")
	}
	if DeclarationsEqual(a, Tool{Name: "read2", Description: "d", Parameters: schemaA}) {
		t.Fatal("name change must compare unequal")
	}
	if DeclarationsEqual(a, Tool{Name: "read", Description: "d"}) {
		t.Fatal("schema presence change must compare unequal")
	}
}

func TestDiffTools(t *testing.T) {
	prev := []Tool{
		{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "bash"},
		{Name: "old"},
	}
	cur := []Tool{
		{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "bash", Description: "now with timeout"},
		{Name: "edit"},
	}
	d := DiffTools(prev, cur)
	if len(d.ToolsAdded) != 2 || d.ToolsAdded[0].Name != "bash" || d.ToolsAdded[1].Name != "edit" {
		t.Fatalf("ToolsAdded = %v", toolNames(d.ToolsAdded))
	}
	// A changed declaration is remove+add — name-referencing transports
	// can only replay it that way.
	if len(d.ToolsRemoved) != 2 || d.ToolsRemoved[0] != "bash" || d.ToolsRemoved[1] != "old" {
		t.Fatalf("ToolsRemoved = %v", d.ToolsRemoved)
	}
}

func TestTranscriptValidation(t *testing.T) {
	if _, err := NewTranscript(userMsg("no leading system")); err == nil {
		t.Fatal("transcript without leading system must be rejected")
	}
	if _, err := NewTranscript(func() Message {
		m := leadingSystem("p")
		m.TS = 12345
		return m
	}()); err == nil {
		t.Fatal("leading system with ts != 0 must be rejected")
	}
	tr := mustTranscript(t, leadingSystem("p"))
	if err := tr.Append(Message{Role: "bogus", Content: []Block{TextBlock("x")}}); err == nil {
		t.Fatal("unknown role must be rejected")
	}
	if err := tr.Append(Message{Role: RoleUser}); err == nil {
		t.Fatal("empty user message must be rejected")
	}
}

func TestNormalizeContext(t *testing.T) {
	tr, err := NormalizeContext(Context{
		SystemPrompt: "p",
		Tools:        []Tool{{Name: "read"}},
		Messages:     []Message{userMsg("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	msgs := tr.Messages()
	if len(msgs) != 2 || msgs[0].Role != RoleSystem || msgs[0].TS != 0 {
		t.Fatalf("normalized shape wrong: %+v", msgs)
	}
	if _, err := NormalizeContext(Context{
		SystemPrompt: "p",
		Messages:     []Message{leadingSystem("own")},
	}); err == nil {
		t.Fatal("prompt + leading system message conflict must be rejected")
	}
	// History that already carries a declaration passes through unchanged.
	tr2, err := NormalizeContext(Context{Messages: []Message{leadingSystem("own"), userMsg("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	if tr2.Len() != 2 {
		t.Fatalf("passthrough changed length: %d", tr2.Len())
	}
}

func TestCanonicalBytesDeterministic(t *testing.T) {
	mk := func() []Message {
		return []Message{
			leadingSystem("p", Tool{Name: "a", Parameters: json.RawMessage(`{"z":1,"a":2}`)}),
			userMsg("hi"),
			assistantMsg("hello"),
		}
	}
	b1, err := CanonicalBytes(mk())
	if err != nil {
		t.Fatal(err)
	}
	b2, err := CanonicalBytes(mk())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatal("canonical serialization is not deterministic")
	}
}

func TestUsageAccounting(t *testing.T) {
	u := Usage{Input: 1, CacheRead: 2, CacheWrite: 3, CacheWrite1h: 4, Output: 5}.Add(
		Usage{Input: 10, CacheRead: 20, Output: 50})
	if u.Input != 11 || u.CacheRead != 22 || u.CacheWrite != 3 || u.CacheWrite1h != 4 || u.Output != 55 {
		t.Fatalf("Add wrong: %+v", u)
	}
	// The 1h bucket is a subset of CacheWrite on providers that report
	// both — it must not be double-counted.
	if u.TotalTokens() != 11+22+3+55 {
		t.Fatalf("TotalTokens = %d", u.TotalTokens())
	}
}

func TestUsageCostAndTotal(t *testing.T) {
	p := Pricing{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}
	u := Usage{Input: 1_000_000, Output: 100_000, CacheRead: 2_000_000, CacheWrite: 500_000}
	cost := u.Cost(p)
	want := 3.0 + 1.5 + 0.6 + 1.875
	if cost < want-1e-9 || cost > want+1e-9 {
		t.Fatalf("cost = %v, want %v", cost, want)
	}
	// 1h bucket folds into the cache-write rate, not double-counted in
	// TotalTokens.
	u2 := Usage{CacheWrite: 100, CacheWrite1h: 100}
	if u2.TotalTokens() != 100 {
		t.Fatalf("TotalTokens double-counts 1h writes: %d", u2.TotalTokens())
	}
	// Callers stamp CostUSD (cli does it at persist time); Add then
	// accumulates stamped values.
	u.CostUSD = cost
	sum := u.Add(Usage{CostUSD: 1.0})
	wantSum := cost + 1.0
	if sum.CostUSD < wantSum-1e-9 || sum.CostUSD > wantSum+1e-9 {
		t.Fatalf("Add lost cost: %v, want %v", sum.CostUSD, wantSum)
	}
}

func TestToolCallBlockHelper(t *testing.T) {
	b := ToolCallBlock("t1", "bash")
	if b.Kind != BlockToolCall || b.ID != "t1" || b.Name != "bash" {
		t.Fatalf("helper wrong: %+v", b)
	}
}
