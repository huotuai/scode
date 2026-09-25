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

	sess, err := s.Create("sess-1", "/proj", "anthropic", "claude-x")
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
	if rec.Header.ID != "sess-1" || rec.Header.CWD != "/proj" || rec.Header.V != formatVersion {
		t.Fatalf("header = %+v", rec.Header)
	}
	tr, err := rec.Transcript()
	if err != nil {
		t.Fatal(err)
	}
	if tr.Len() != 3 || tr.Messages()[1].Content[0].Text != "hi" {
		t.Fatalf("messages = %+v", tr.Messages())
	}
}

func TestSessionTruncatedTailTolerated(t *testing.T) {
	s := newTestStore(t)
	sess, err := s.Create("sess-2", "/p", "", "")
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
	if len(rec.Entries) != 2 {
		t.Fatalf("entries = %d, want 2 (intact prefix)", len(rec.Entries))
	}
}

func TestSessionListNewestFirst(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"20260101-090000-0001", "20260201-090000-0002", "20260115-090000-0003"} {
		sess, err := s.Create(id, "/p", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.Close(); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || ids[0] != "20260201-090000-0002" || ids[2] != "20260101-090000-0001" {
		t.Fatalf("ids = %v", ids)
	}
}

// OpenForAppend: resumed sessions keep appending to the original file,
// and the id stays resumable across generations.
// A crash-truncated final line (no newline) must not swallow entries
// appended afterwards: OpenForAppend repairs the tail before writing.
func TestOpenForAppendRepairsTornTail(t *testing.T) {
	s := newTestStore(t)
	sess, err := s.Create("torn", "/p", "", "")
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
	if err := s2.Append(llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("after-crash")}, TS: 2}); err != nil {
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
	for _, e := range rec.Entries {
		if e.Msg != nil && len(e.Msg.Content) > 0 && e.Msg.Content[0].Text == "after-crash" {
			found = true
		}
	}
	if !found {
		t.Fatalf("post-crash entry lost; entries = %d", len(rec.Entries))
	}
}

// Fork clones entries into a new id; upto trims mid-session.
func TestForkCloneAndTrim(t *testing.T) {
	s := newTestStore(t)
	sess, err := s.Create("orig", "/p", "", "")
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
	if len(rec.Entries) != 4 {
		t.Fatalf("fork entries = %d, want 4", len(rec.Entries))
	}
	// Original untouched.
	orig, _ := s.Load("orig")
	if len(orig.Entries) != 4 {
		t.Fatalf("original mutated: %d", len(orig.Entries))
	}

	trimID, err := s.Fork("orig", 3)
	if err != nil {
		t.Fatal(err)
	}
	rec2, _ := s.Load(trimID)
	if len(rec2.Entries) != 3 {
		t.Fatalf("trimmed fork entries = %d, want 3", len(rec2.Entries))
	}
	// Trimmed fork stays resumable (leading system first).
	if _, err := rec2.Transcript(); err != nil {
		t.Fatalf("trimmed fork not resumable: %v", err)
	}
}

func TestOpenForAppendChain(t *testing.T) {
	s := newTestStore(t)
	sess, err := s.Create("gen", "/p", "", "")
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
	if err := s2.Append(llm.Message{Role: llm.RoleAssistant, StopReason: llm.StopEndTurn, Content: []llm.Block{llm.TextBlock("a1")}, TS: 2}); err != nil {
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
	if err := s3.Append(llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("u2")}, TS: 3}); err != nil {
		t.Fatal(err)
	}
	if err := s3.Close(); err != nil {
		t.Fatal(err)
	}

	rec, err := s.Load("gen")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Entries) != 4 {
		t.Fatalf("entries = %d, want 4 (single file, full history)", len(rec.Entries))
	}
	tr, err := rec.Transcript()
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
