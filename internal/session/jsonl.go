// Package session persists transcripts as append-only JSONL files (pi's
// format in spirit): a header line carrying identity, then one message
// per line. Files are never rewritten during a conversation — crash
// safety and cache stability fall out of the same discipline.
package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"scode/internal/llm"
)

const formatVersion = 1

// Header is the first line of a session file.
type Header struct {
	V         int    `json:"v"`
	Kind      string `json:"kind"` // "header"
	ID        string `json:"id"`
	CreatedAt int64  `json:"createdAt"`
	CWD       string `json:"cwd"`
	Model     string `json:"model,omitempty"`
	Provider  string `json:"provider,omitempty"`
}

// Store manages session files under a root directory.
type Store struct {
	Root string
	Now  func() time.Time
}

// NewStore creates the session root (and its parents).
func NewStore(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Store{Root: root, Now: time.Now}, nil
}

// DefaultRoot is <configDir>/sessions/<safe-cwd>.
func DefaultRoot(configDir, cwd string) string {
	safe := strings.NewReplacer(":", "-", "\\", "-", "/", "-", " ", "_").Replace(filepath.Clean(cwd))
	safe = strings.TrimPrefix(strings.TrimPrefix(safe, "-"), ".")
	if safe == "" {
		safe = "default"
	}
	return filepath.Join(configDir, "sessions", safe)
}

// NewID produces a time-sortable session id (date + counter).
func NewID(t time.Time) string {
	return t.Format("20060102-150405") + fmt.Sprintf("-%04d", t.Nanosecond()%10000)
}

// Create starts a new session file with a header.
func (s *Store) Create(id, cwd, provider, model string) (*Session, error) {
	if id == "" {
		id = NewID(s.Now())
	}
	h := Header{
		V: formatVersion, Kind: "header", ID: id,
		CreatedAt: s.Now().UnixMilli(), CWD: cwd,
		Provider: provider, Model: model,
	}
	path := filepath.Join(s.Root, id+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(h)
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	return &Session{Path: path, store: s, w: bufio.NewWriter(f), f: f, header: h}, nil
}

// Session is an open append-only session file.
type Session struct {
	Path   string
	store  *Store
	w      *bufio.Writer
	f      *os.File
	header Header
	count  int
}

// Append writes one message line and flushes.
func (s *Session) Append(m llm.Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err := s.w.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := s.w.Flush(); err != nil {
		return err
	}
	s.count++
	return s.f.Sync() // crash safety: every turn is durable
}

// AppendAll writes several messages in order.
func (s *Session) AppendAll(msgs []llm.Message) error {
	for _, m := range msgs {
		if err := s.Append(m); err != nil {
			return err
		}
	}
	return nil
}

// Close flushes and closes the file.
func (s *Session) Close() error {
	if err := s.w.Flush(); err != nil {
		s.f.Close()
		return err
	}
	return s.f.Close()
}

// Header returns the session's header.
func (s *Session) Header() Header { return s.header }

// Record couples a loaded session with its transcript.
type Record struct {
	Header   Header
	Messages []llm.Message
}

// Load reads a session file (used by --resume).
func (s *Store) Load(id string) (*Record, error) {
	path := filepath.Join(s.Root, id+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 32<<20)

	rec := &Record{}
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if first {
			first = false
			if err := json.Unmarshal([]byte(line), &rec.Header); err != nil {
				return nil, fmt.Errorf("header: %w", err)
			}
			continue
		}
		var m llm.Message
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			// Truncated tail after a crash: keep everything intact so far.
			break
		}
		rec.Messages = append(rec.Messages, m)
	}
	if first {
		return nil, fmt.Errorf("empty session file %s", path)
	}
	return rec, sc.Err()
}

// Transcript rebuilds the in-memory transcript from the record.
func (r *Record) Transcript() (*llm.Transcript, error) {
	if len(r.Messages) == 0 {
		return nil, fmt.Errorf("session has no messages")
	}
	return llm.NewTranscript(r.Messages[0], r.Messages[1:]...)
}

// List returns available session ids, newest first.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(e.Name(), ".jsonl"))
	}
	// IDs start with a sortable timestamp: reverse for newest-first.
	for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
		ids[i], ids[j] = ids[j], ids[i]
	}
	return ids, nil
}
