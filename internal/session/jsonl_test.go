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
