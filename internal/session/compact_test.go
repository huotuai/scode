package session

import (
	"encoding/json"
	"strings"
	"testing"

	"scode/internal/llm"
)

func sysEntry() Entry {
	m := llm.Message{Role: llm.RoleSystem, Content: []llm.Block{llm.TextBlock("sys")}, ToolsAdded: []llm.Tool{{Name: "read"}}}
	return MsgEntry(m)
}

func userEntry(text string, ts int64) Entry {
	return MsgEntry(llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock(text)}, TS: ts})
}

func TestProjectWithoutMarker(t *testing.T) {
	entries := []Entry{sysEntry(), userEntry("a", 1), userEntry("b", 2)}
	msgs := Project(entries)
	if len(msgs) != 3 || msgs[1].Content[0].Text != "a" {
		t.Fatalf("msgs = %+v", msgs)
	}
}

func TestProjectWithMarker(t *testing.T) {
	marker := NewCompaction("the task was X, state is Y", []string{"a.go"}, []string{"b.go"})
	entries := []Entry{
		sysEntry(),
		userEntry("old1", 1),
		userEntry("old2", 2),
		{Compaction: &marker},
		userEntry("new1", 3),
	}
	msgs := Project(entries)
	if len(msgs) != 3 {
		t.Fatalf("len = %d, want 3 (system, summary, tail): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != llm.RoleSystem {
		t.Fatalf("leading = %+v", msgs[0])
	}
	summary := msgs[1]
	if summary.Role != llm.RoleUser {
		t.Fatalf("summary role = %s", summary.Role)
	}
	text := summary.Content[0].Text
	for _, want := range []string{"compacted", "the task was X", "Files read so far: a.go", "Files modified so far: b.go"} {
		if !strings.Contains(text, want) {
			t.Fatalf("summary missing %q: %q", want, text)
		}
	}
	if msgs[2].Content[0].Text != "new1" {
		t.Fatalf("tail = %+v", msgs[2])
	}
}

func TestProjectOnlyNewestMarkerCounts(t *testing.T) {
	old := NewCompaction("first summary", nil, nil)
	newest := NewCompaction("second summary", nil, nil)
	entries := []Entry{
		sysEntry(),
		{Compaction: &old},
		userEntry("middle", 1),
		{Compaction: &newest},
		userEntry("tail", 2),
	}
	msgs := Project(entries)
	if len(msgs) != 3 {
		t.Fatalf("len = %d", len(msgs))
	}
	if !strings.Contains(msgs[1].Content[0].Text, "second summary") || strings.Contains(msgs[1].Content[0].Text, "first summary") {
		t.Fatalf("wrong summary used: %q", msgs[1].Content[0].Text)
	}
	if msgs[2].Content[0].Text != "tail" {
		t.Fatalf("tail after newest marker wrong: %+v", msgs[2])
	}
}

func TestNeedsCompaction(t *testing.T) {
	entries := []Entry{sysEntry(), userEntry("hi", 1)}
	if NeedsCompaction(entries, 1000) {
		t.Fatal("no usage reported: never compacts")
	}
	asst := llm.Message{
		Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, TS: 2,
		Content: []llm.Block{llm.TextBlock("ok")},
		Usage:   &llm.Usage{Input: 30_000, CacheRead: 50_000, Output: 2_000},
	}
	entries = append(entries, MsgEntry(asst))
	if !NeedsCompaction(entries, 80_000) {
		t.Fatal("82k context must trigger at 80k threshold")
	}
	if NeedsCompaction(entries, 200_000) {
		t.Fatal("82k context must not trigger at 200k threshold")
	}
	if NeedsCompaction(entries, -1) {
		t.Fatal("negative threshold disables compaction")
	}
}

func asstToolCall(name string, args string) llm.Message {
	return llm.Message{
		Role: llm.RoleAssistant, StopReason: llm.StopToolUse, TS: 1,
		Content: []llm.Block{{Kind: llm.BlockToolCall, ID: "c", Name: name, Arguments: json.RawMessage(args)}},
	}
}

func TestExtractFileOps(t *testing.T) {
	msgs := []llm.Message{
		asstToolCall("read", `{"path":"a.go"}`),
		asstToolCall("edit", `{"path":"b.go"}`),
		asstToolCall("write", `{"path":"c.txt"}`),
		asstToolCall("grep", `{"pattern":"x","path":"d/"}`),
		asstToolCall("bash", `{"command":"ls"}`),
		asstToolCall("read", `{"path":"a.go"}`), // dedup
	}
	ops := ExtractFileOps(msgs, FileOps{Read: []string{"old.go"}})
	if strings.Join(ops.Read, ",") != "a.go,d/,old.go" {
		t.Fatalf("read = %v", ops.Read)
	}
	if strings.Join(ops.Modified, ",") != "b.go,c.txt" {
		t.Fatalf("modified = %v", ops.Modified)
	}
}

func TestExtractFileOpsBadArgs(t *testing.T) {
	ops := ExtractFileOps([]llm.Message{asstToolCall("read", `not json`)}, FileOps{})
	if len(ops.Read) != 0 {
		t.Fatalf("read = %v", ops.Read)
	}
}

func TestBuildSummaryPrompt(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: []llm.Block{llm.TextBlock("sys prompt excluded")}},
		{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("fix the bug")}, TS: 1},
		asstToolCall("read", `{"path":"main.go"}`),
		{Role: llm.RoleTool, Content: []llm.Block{{Kind: llm.BlockToolResult, ID: "c", IsError: true, Content: []llm.Block{llm.TextBlock("boom")}}}, TS: 2},
		{Role: llm.RoleAssistant, Content: []llm.Block{{Kind: llm.BlockThinking, Text: "inner thoughts"}}, TS: 3},
	}
	p := BuildSummaryPrompt(msgs, FileOps{Read: []string{"carried.go"}})
	for _, want := range []string{"fix the bug", "[tool call read", "[tool error] boom", "Files read: carried.go"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
	if strings.Contains(p, "inner thoughts") {
		t.Fatal("thinking blocks must not be summarized")
	}
	if strings.Contains(p, "sys prompt excluded") {
		t.Fatal("system prompt must not be summarized")
	}
}

func TestJSONLRoundTripWithCompaction(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := s.Create("c1", "/p", "anthropic", "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendAll([]llm.Message{
		{Role: llm.RoleSystem, Content: []llm.Block{llm.TextBlock("s")}},
		{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("u1")}, TS: 1},
	}); err != nil {
		t.Fatal(err)
	}
	marker := NewCompaction("summary text", []string{"x.go"}, nil)
	if err := sess.AppendEntry(Entry{Compaction: &marker}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("u2")}, TS: 2}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	rec, err := s.Load("c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Entries) != 4 {
		t.Fatalf("entries = %d", len(rec.Entries))
	}
	if rec.Entries[2].Compaction == nil || rec.Entries[2].Compaction.Summary != "summary text" {
		t.Fatalf("marker entry = %+v", rec.Entries[2])
	}
	ops := rec.LatestFileOps()
	if len(ops.Read) != 1 || ops.Read[0] != "x.go" {
		t.Fatalf("LatestFileOps = %+v", ops)
	}
	msgs := rec.Project()
	if len(msgs) != 3 { // system, summary-user, u2
		t.Fatalf("projected = %d msgs: %+v", len(msgs), msgs)
	}
	if msgs[2].Content[0].Text != "u2" {
		t.Fatalf("tail = %+v", msgs[2])
	}
	tr, err := rec.Transcript()
	if err != nil {
		t.Fatal(err)
	}
	if tr.Len() != 3 {
		t.Fatalf("transcript len = %d", tr.Len())
	}
}

func TestMessageLineWithoutRoleRejected(t *testing.T) {
	if _, err := parseEntry(`{"ts":123}`); err == nil {
		t.Fatal("role-less line must not parse as a message entry")
	}
	if _, err := parseEntry(`{"kind":"compaction","summary":"s"}`); err != nil {
		t.Fatalf("compaction line must parse: %v", err)
	}
	if _, err := parseEntry(`{"role":"user","content":[{"kind":"text","text":"x"}]}`); err != nil {
		t.Fatalf("message line must parse: %v", err)
	}
}
