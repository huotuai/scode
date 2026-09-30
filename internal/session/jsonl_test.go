package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"scode/internal/llm"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSessionRoundTrip(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }

	sess, err := s.Create("sess-1", "/proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendAll([]llm.Message{
		{Role: llm.RoleSystem, Content: []llm.Block{llm.TextBlock("sys")}},
		{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("hi")}, TS: 1},
		{Role: llm.RoleAssistant, Content: []llm.Block{llm.TextBlock("hello")}, StopReason: llm.StopEndTurn, TS: 2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	rec, err := s.Load("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Header.ID != "sess-1" || rec.Header.CWD != "/proj" || rec.Header.Version != formatVersion {
		t.Fatalf("header = %+v", rec.Header)
	}
	tr, err := rec.Transcript(testSysMsg())
	if err != nil {
		t.Fatal(err)
	}
	if tr.Len() != 3 || tr.Messages()[1].Content[0].Text != "hi" {
		t.Fatalf("messages = %+v", tr.Messages())
	}
}

// Model switches round-trip as append-only entries; the last one wins
// and never enters the projected conversation.
func TestModelEntryRoundTrip(t *testing.T) {
	s := newTestStore(t)
	sess, err := s.Create("sess-model", "/p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.AppendEntry(Entry{Model: &ModelEntry{Provider: "kimi", Model: "k3"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.AppendEntry(Entry{Model: &ModelEntry{Provider: "openai-compat", Model: "deepseek-flash"}}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendAll([]llm.Message{{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("hi")}, TS: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	rec, err := s.Load("sess-model")
	if err != nil {
		t.Fatal(err)
	}
	p, m := CurrentModel(rec.Path())
	if p != "openai-compat" || m != "deepseek-flash" {
		t.Fatalf("CurrentModel = %q/%q, want last entry", p, m)
	}
	// Model entries are log-only: the projection ignores them.
	if msgs := Project(rec.Path()); len(msgs) != 1 || msgs[0].Role != llm.RoleUser {
		t.Fatalf("projection = %+v, want the single user message", msgs)
	}
}

func TestSessionTruncatedTailTolerated(t *testing.T) {
	s := newTestStore(t)
	sess, err := s.Create("sess-2", "/p")
	if err != nil {
		t.Fatal(err)
	}
	sess.AppendAll([]llm.Message{ //nolint:errcheck
		{Role: llm.RoleSystem, Content: []llm.Block{llm.TextBlock("s")}},
		{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("u")}, TS: 1},
	})
	sess.Close() //nolint:errcheck

	// Simulate a crash mid-write: append a half line.
	path := filepath.Join(s.Root, "sess-2.jsonl")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"role":"assistant","content":[{"kind":"text","text":"bro`) //nolint:errcheck
	f.Close()                                                                  //nolint:errcheck

	rec, err := s.Load("sess-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Path()) != 2 {
		t.Fatalf("entries = %d, want 2 (intact prefix)", len(rec.Path()))
	}
}

// List sorts by last activity (mtime), newest first (pi's ordering) —
// not by id.
func TestSessionListNewestFirst(t *testing.T) {
	s := newTestStore(t)
	ids3 := []string{"20260101-090000-0001", "20260201-090000-0002", "20260115-090000-0003"}
	for _, id := range ids3 {
		sess, err := s.Create(id, "/p")
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// Deterministic mtimes: 0003 newest, 0002 middle, 0001 oldest —
	// deliberately NOT the id order.
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for i, id := range ids3 {
		os.Chtimes(filepath.Join(s.Root, id+".jsonl"), base, base.Add(time.Duration(i)*time.Hour)) //nolint:errcheck
	}
	ids, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || ids[0] != "20260115-090000-0003" || ids[1] != "20260201-090000-0002" || ids[2] != "20260101-090000-0001" {
		t.Fatalf("ids = %v", ids)
	}
}

// OpenForAppend: resumed sessions keep appending to the original file,
// and the id stays resumable across generations.
// A crash-truncated final line (no newline) must not swallow entries
// appended afterwards: OpenForAppend repairs the tail before writing.
func TestOpenForAppendRepairsTornTail(t *testing.T) {
	s := newTestStore(t)
	sess, err := s.Create("torn", "/p")
	if err != nil {
		t.Fatal(err)
	}
	sess.AppendAll([]llm.Message{ //nolint:errcheck
		{Role: llm.RoleSystem, Content: []llm.Block{llm.TextBlock("s")}},
		{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("u1")}, TS: 1},
	})
	sess.Close() //nolint:errcheck

	// Simulate a crash mid-write: half a line, no newline.
	path := filepath.Join(s.Root, "torn.jsonl")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"role":"assistant","content":[{"kind":"text","text":"bro`) //nolint:errcheck
	f.Close()                                                                  //nolint:errcheck

	s2, err := s.OpenForAppend("torn")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Append(llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("after-crash")}, TS: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}

	// The post-crash entry survives: the torn fragment was isolated by
	// the repair newline instead of merging with it.
	rec, err := s.Load("torn")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range rec.Path() {
		if e.Msg != nil && len(e.Msg.Content) > 0 && e.Msg.Content[0].Text == "after-crash" {
			found = true
		}
	}
	if !found {
		t.Fatalf("post-crash entry lost; entries = %d", len(rec.Path()))
	}
}

// Fork clones entries into a new id; upto trims mid-session.
func TestForkCloneAndTrim(t *testing.T) {
	s := newTestStore(t)
	sess, err := s.Create("orig", "/p")
	if err != nil {
		t.Fatal(err)
	}
	sess.AppendAll([]llm.Message{ //nolint:errcheck
		{Role: llm.RoleSystem, Content: []llm.Block{llm.TextBlock("s")}},
		{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("u1")}, TS: 1},
		{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("a1")}, TS: 2},
		{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("u2")}, TS: 3},
	})
	sess.Close() //nolint:errcheck

	forkID, err := s.Fork("orig", 0)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.Load(forkID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Path()) != 4 {
		t.Fatalf("fork entries = %d, want 4", len(rec.Path()))
	}
	// Original untouched.
	orig, _ := s.Load("orig")
	if len(orig.Path()) != 4 {
		t.Fatalf("original mutated: %d", len(orig.Path()))
	}

	trimID, err := s.Fork("orig", 3)
	if err != nil {
		t.Fatal(err)
	}
	rec2, _ := s.Load(trimID)
	if len(rec2.Path()) != 3 {
		t.Fatalf("trimmed fork entries = %d, want 3", len(rec2.Path()))
	}
	// Trimmed fork stays resumable (leading system first).
	if _, err := rec2.Transcript(testSysMsg()); err != nil {
		t.Fatalf("trimmed fork not resumable: %v", err)
	}
}

func TestOpenForAppendChain(t *testing.T) {
	s := newTestStore(t)
	sess, err := s.Create("gen", "/p")
	if err != nil {
		t.Fatal(err)
	}
	sess.AppendAll([]llm.Message{ //nolint:errcheck
		{Role: llm.RoleSystem, Content: []llm.Block{llm.TextBlock("s")}},
		{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("u1")}, TS: 1},
	})
	sess.Close() //nolint:errcheck

	// First resume generation: append more.
	s2, err := s.OpenForAppend("gen")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Append(llm.Message{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("a1")}, TS: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}

	// Second resume generation still works, full history intact.
	s3, err := s.OpenForAppend("gen")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s3.Append(llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("u2")}, TS: 3}); err != nil {
		t.Fatal(err)
	}
	if err := s3.Close(); err != nil {
		t.Fatal(err)
	}

	rec, err := s.Load("gen")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Path()) != 4 {
		t.Fatalf("entries = %d, want 4 (single file, full history)", len(rec.Path()))
	}
	tr, err := rec.Transcript(testSysMsg())
	if err != nil {
		t.Fatal(err)
	}
	if tr.Len() != 4 || tr.Messages()[3].Content[0].Text != "u2" {
		t.Fatalf("transcript = %+v", tr.Messages())
	}
}

func TestDefaultRootSanitizes(t *testing.T) {
	root := DefaultRoot(filepath.Join(t.TempDir(), "cfg"), `E:\ai\harness\SCode`)
	seg := filepath.Base(root)
	if strings.ContainsAny(seg, `:\`) {
		t.Fatalf("unsafe segment %q in %q", seg, root)
	}
	if !strings.Contains(seg, "SCode") {
		t.Fatalf("cwd not recognizable in %q", seg)
	}
}

// Tree resolution (pi's buildSessionPath): a branched file resolves the
// PATH along the leaf's parent chain, not file order.
func TestTreePathFollowsLeafBranch(t *testing.T) {
	s := newTestStore(t)
	sess, err := s.Create("tree", "/p")
	if err != nil {
		t.Fatal(err)
	}
	// main branch: root -> a1
	root, _ := sess.Append(llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("root")}, TS: 1})
	a1, _ := sess.Append(llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("a1")}, TS: 2})
	// branch off root: b1 -> b2 (leaf is on the b branch)
	b1 := Entry{Type: "message", ParentID: root.ID, Msg: &llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("b1")}, TS: 3}}
	if _, err := sess.AppendEntry(b1); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Append(llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("b2")}, TS: 4}); err != nil {
		t.Fatal(err)
	}
	sess.Close() //nolint:errcheck

	rec, err := s.Load("tree")
	if err != nil {
		t.Fatal(err)
	}
	path := rec.Path()
	var texts []string
	for _, e := range path {
		texts = append(texts, e.Msg.Content[0].Text)
	}
	if strings.Join(texts, ",") != "root,b1,b2" {
		t.Fatalf("path = %v, want the b branch", texts)
	}
	_ = a1
}

// Legacy v1 files (bare message lines, kind-tagged header and markers)
// migrate on read: linear ids/parents assigned, index-based kept
// ranges converted to entry ids.
func TestLegacyV1Migration(t *testing.T) {
	s := newTestStore(t)
	kept := 1
	lines := []string{
		`{"v":1,"kind":"header","id":"legacy","createdAt":1700000000000,"cwd":"/p"}`,
		`{"role":"user","content":[{"kind":"text","text":"old"}],"ts":1}`,
		`{"role":"user","content":[{"kind":"text","text":"kept"}],"ts":2}`,
		`{"kind":"compaction","summary":"s","firstKeptIndex":1,"createdAt":1700000001000}`,
		`{"role":"user","content":[{"kind":"text","text":"new"}],"ts":3}`,
	}
	_ = kept
	os.WriteFile(filepath.Join(s.Root, "legacy.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644) //nolint:errcheck

	rec, err := s.Load("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Header.Version != 1 || rec.Header.ID != "legacy" {
		t.Fatalf("header = %+v", rec.Header)
	}
	path := rec.Path()
	if len(path) != 4 {
		t.Fatalf("path = %d", len(path))
	}
	for i, e := range path {
		wantParent := ""
		if i > 0 {
			wantParent = path[i-1].ID
		}
		if e.ID == "" || e.ParentID != wantParent {
			t.Fatalf("entry %d lacks migration links: %+v", i, e)
		}
	}
	// The migrated kept range resolves to the second entry's id.
	c := path[2].Compaction
	if c == nil || c.FirstKeptEntryID != path[1].ID {
		t.Fatalf("migrated marker = %+v, want firstKeptEntryId=%s", c, path[1].ID)
	}
	// Projection honors the kept range: summary + kept + new.
	msgs := rec.Project()
	if len(msgs) != 3 || msgs[1].Content[0].Text != "kept" || msgs[2].Content[0].Text != "new" {
		t.Fatalf("projected = %+v", msgs)
	}
}

// pi v2 interop: a file written in pi's exact shape loads, and unknown
// pi entry types stay in the tree without reaching the projection.
func TestPiV2Interop(t *testing.T) {
	s := newTestStore(t)
	lines := []string{
		`{"type":"session","version":3,"id":"pi-file","timestamp":"2026-01-01T00:00:00Z","cwd":"/p"}`,
		`{"type":"message","id":"aaa00001","parentId":"","timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"from pi"}],"timestamp":1}}`,
		`{"type":"model_change","id":"aaa00002","parentId":"aaa00001","timestamp":"2026-01-01T00:00:02Z","provider":"anthropic","modelId":"claude"}`,
		`{"type":"message","id":"aaa00003","parentId":"aaa00002","timestamp":"2026-01-01T00:00:03Z","message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"timestamp":2,"stopReason":"stop"}}`,
	}
	os.WriteFile(filepath.Join(s.Root, "pi-file.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644) //nolint:errcheck

	rec, err := s.Load("pi-file")
	if err != nil {
		t.Fatal(err)
	}
	path := rec.Path()
	if len(path) != 3 { // model_change stays in the tree
		t.Fatalf("path = %d, want 3", len(path))
	}
	msgs := rec.Project()
	if len(msgs) != 2 || msgs[0].Content[0].Text != "from pi" {
		t.Fatalf("projected = %+v", msgs)
	}
}

// Fork links the clone back via parentSession and remaps kept-range ids.
func TestForkParentSessionAndKeptRemap(t *testing.T) {
	s := newTestStore(t)
	sess, err := s.Create("src", "/p")
	if err != nil {
		t.Fatal(err)
	}
	e1, _ := sess.Append(llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("one")}, TS: 1})
	e2, _ := sess.Append(llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("two")}, TS: 2})
	marker := NewCompaction("s", nil, nil)
	marker.FirstKeptEntryID = e2.ID
	if _, err := sess.AppendEntry(Entry{Compaction: &marker}); err != nil {
		t.Fatal(err)
	}
	sess.Close() //nolint:errcheck

	forkID, err := s.Fork("src", 0)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.Load(forkID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Header.ParentSession == "" {
		t.Fatal("clone header lacks parentSession")
	}
	path := rec.Path()
	c := path[2].Compaction
	if c.FirstKeptEntryID != path[1].ID || c.FirstKeptEntryID == e2.ID {
		t.Fatalf("kept id not remapped: %q (want the clone's %q)", c.FirstKeptEntryID, path[1].ID)
	}
	if path[1].Msg.Content[0].Text != "two" {
		t.Fatalf("remap points at wrong entry: %+v", path[1])
	}
	_ = e1
}

// Delete is idempotent and rejects path-traversal ids; HasMessages
// distinguishes a header-only session from one with conversation.
func TestStoreDeleteAndHasMessages(t *testing.T) {
	s := newTestStore(t)

	empty, err := s.Create("empty-1", "/p")
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
	if has, err := s.HasMessages("empty-1"); err != nil || has {
		t.Fatalf("empty session: has=%v err=%v", has, err)
	}

	chat, err := s.Create("chat-1", "/p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chat.Append(llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("hi")}}); err != nil {
		t.Fatal(err)
	}
	if err := chat.Close(); err != nil {
		t.Fatal(err)
	}
	if has, err := s.HasMessages("chat-1"); err != nil || !has {
		t.Fatalf("chat session: has=%v err=%v", has, err)
	}

	if err := s.Delete("chat-1"); err != nil {
		t.Fatal(err)
	}
	ids, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id == "chat-1" {
			t.Fatalf("deleted session still listed: %v", ids)
		}
	}

	// Idempotent + traversal-safe.
	if err := s.Delete("chat-1"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if err := s.Delete("../escape"); err == nil {
		t.Fatal("expected error for traversal id")
	}
	if err := s.Delete(""); err == nil {
		t.Fatal("expected error for empty id")
	}
}

// Title entries are log-only markers: they fold last-wins, survive a load,
// and never project into the LLM conversation.
func TestTitleEntryRoundTrip(t *testing.T) {
	s := newTestStore(t)
	sess, err := s.Create("titled", "/p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Append(llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("hi")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.AppendEntry(Entry{Title: &TitleEntry{Title: "old"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.AppendEntry(Entry{Title: &TitleEntry{Title: "新标题"}}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	rec, err := s.Load("titled")
	if err != nil {
		t.Fatal(err)
	}
	if got := CurrentTitle(rec.Path()); got != "新标题" {
		t.Fatalf("title = %q, want 新标题", got)
	}
	if msgs := rec.Project(); len(msgs) != 1 {
		t.Fatalf("project = %d messages, want 1 (title must not project)", len(msgs))
	}
}

func TestPlanEntryRoundTrip(t *testing.T) {
	s := newTestStore(t)
	sess, err := s.Create("planned", "/p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Append(llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("hi")}}); err != nil {
		t.Fatal(err)
	}
	first := &PlanEntry{Items: []PlanItem{
		{Step: "调研", Status: "in_progress"},
		{Step: "实现", Status: "pending"},
	}}
	if _, err := sess.AppendEntry(Entry{Plan: first}); err != nil {
		t.Fatal(err)
	}
	// Whole-value replace: the second entry supersedes the first.
	second := &PlanEntry{Explanation: "调研完成", Items: []PlanItem{
		{Step: "调研", Status: "completed"},
		{Step: "实现", Status: "in_progress"},
	}}
	if _, err := sess.AppendEntry(Entry{Plan: second}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	rec, err := s.Load("planned")
	if err != nil {
		t.Fatal(err)
	}
	got := CurrentPlan(rec.Path())
	if got == nil {
		t.Fatal("plan = nil, want the last plan entry")
	}
	if got.Explanation != "调研完成" || len(got.Items) != 2 ||
		got.Items[0].Status != "completed" || got.Items[1].Status != "in_progress" {
		t.Fatalf("plan = %+v, want the last entry's state", got)
	}
	if msgs := rec.Project(); len(msgs) != 1 {
		t.Fatalf("project = %d messages, want 1 (plan must not project)", len(msgs))
	}
}

func TestTransientWriteError(t *testing.T) {
	if transientWriteError(nil) {
		t.Fatal("nil must not be transient")
	}
	if !transientWriteError(os.ErrPermission) {
		t.Fatal("permission error should be transient")
	}
	if transientWriteError(os.ErrNotExist) {
		t.Fatal("not-exist error must not be transient")
	}
}

// HasMessages must recognize v1 sessions too: the desktop prune sweep
// deletes on this verdict, and a bare-line v1 file with real
// conversation used to read as empty (permanent transcript loss).
func TestHasMessagesLegacyV1(t *testing.T) {
	s := newTestStore(t)
	header := `{"v":1,"kind":"header","id":"x","createdAt":0,"cwd":"/p","provider":"openai-compat","model":"m"}`
	write := func(id string, lines ...string) {
		t.Helper()
		os.WriteFile(filepath.Join(s.Root, id+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644) //nolint:errcheck
	}

	write("v1-chat", header,
		`{"role":"user","content":[{"kind":"text","text":"hello"}]}`,
		`{"role":"assistant","content":[{"kind":"text","text":"hi"}]}`)
	if has, err := s.HasMessages("v1-chat"); err != nil || !has {
		t.Fatalf("v1 session with conversation: has=%v err=%v", has, err)
	}

	write("v1-empty", header)
	if has, err := s.HasMessages("v1-empty"); err != nil || has {
		t.Fatalf("v1 header-only session: has=%v err=%v", has, err)
	}

	// A v1 compaction marker alone must not read as a message.
	write("v1-marker", header,
		`{"kind":"compaction","summary":"summarized the greeting","readFiles":[],"modifiedFiles":[],"createdAt":1}`)
	if has, err := s.HasMessages("v1-marker"); err != nil || has {
		t.Fatalf("v1 marker-only session: has=%v err=%v", has, err)
	}
}

// pi-written sessions tag content blocks with "type" instead of
// "kind": the block kinds must survive the load — an empty Kind
// silently drops tool calls and breaks provider replay on resume.
func TestPiV2InteropBlockKinds(t *testing.T) {
	s := newTestStore(t)
	lines := []string{
		`{"type":"session","version":3,"id":"pi-tools","timestamp":"2026-01-01T00:00:00Z","cwd":"/p"}`,
		`{"type":"message","id":"aaa00001","parentId":"","timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"run it"}]}}`,
		`{"type":"message","id":"aaa00002","parentId":"aaa00001","timestamp":"2026-01-01T00:00:02Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"call_1","name":"bash","arguments":{"command":"ls"}}]}}`,
		`{"type":"message","id":"aaa00003","parentId":"aaa00002","timestamp":"2026-01-01T00:00:03Z","message":{"role":"toolResult","content":[{"type":"toolResult","id":"call_1","content":[{"type":"text","text":"file list"}]}]}}`,
	}
	os.WriteFile(filepath.Join(s.Root, "pi-tools.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644) //nolint:errcheck

	rec, err := s.Load("pi-tools")
	if err != nil {
		t.Fatal(err)
	}
	msgs := rec.Project()
	if len(msgs) != 3 {
		t.Fatalf("projected = %d msgs: %+v", len(msgs), msgs)
	}
	if msgs[0].Content[0].Kind != llm.BlockText || msgs[0].Content[0].Text != "run it" {
		t.Fatalf("user block = %+v", msgs[0].Content[0])
	}
	call := msgs[1].Content[0]
	if call.Kind != llm.BlockToolCall || call.Name != "bash" || string(call.Arguments) != `{"command":"ls"}` {
		t.Fatalf("tool call block = %+v", call)
	}
	res := msgs[2].Content[0]
	if res.Kind != llm.BlockToolResult || res.ID != "call_1" ||
		len(res.Content) != 1 || res.Content[0].Kind != llm.BlockText || res.Content[0].Text != "file list" {
		t.Fatalf("tool result block = %+v", res)
	}
}

// v1 compaction markers carried the working set as flat top-level
// readFiles/modifiedFiles fields; the migration must fold them into
// Details so the carried working set survives a resume instead of
// silently dropping.
func TestLegacyCompactionWorkingSetSurvives(t *testing.T) {
	s := newTestStore(t)
	lines := []string{
		`{"v":1,"kind":"header","id":"x","createdAt":0,"cwd":"/p"}`,
		`{"role":"user","content":[{"kind":"text","text":"hi"}]}`,
		`{"kind":"compaction","summary":"did things","readFiles":["a.go","b.go"],"modifiedFiles":["c.go"],"createdAt":1}`,
	}
	os.WriteFile(filepath.Join(s.Root, "v1-comp.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644) //nolint:errcheck

	rec, err := s.Load("v1-comp")
	if err != nil {
		t.Fatal(err)
	}
	ops := rec.LatestFileOps()
	if len(ops.Read) != 2 || ops.Read[0] != "a.go" || ops.Read[1] != "b.go" ||
		len(ops.Modified) != 1 || ops.Modified[0] != "c.go" {
		t.Fatalf("working set = %+v", ops)
	}
	// The marker still projects, with the carried file lists inlined.
	msgs := rec.Project()
	if len(msgs) != 1 {
		t.Fatalf("projection = %d msgs, want 1 (marker only)", len(msgs))
	}
	text := msgs[0].Content[0].Text
	if !strings.Contains(text, "did things") || !strings.Contains(text, "a.go") {
		t.Fatalf("compaction message = %q", text)
	}
}
