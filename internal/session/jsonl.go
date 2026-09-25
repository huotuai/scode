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

// OpenForAppend reopens an existing session file for continued
// appending — the resume path. The file's history stays append-only and
// self-contained: no continuation files are spun off, so any stored
// session id remains resumable however many times it has been resumed.
// A crash-truncated final line (no trailing newline) is repaired with a
// newline before any append lands — otherwise the first new entry would
// merge into the torn fragment and doom every later entry on load.
func (s *Store) OpenForAppend(id string) (*Session, error) {
	rec, err := s.Load(id)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(s.Root, id+".jsonl")
	// O_RDWR (not O_WRONLY): repairTornTail reads the last byte, and
	// Windows denies reads on write-only handles.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	if err := repairTornTail(f); err != nil {
		f.Close()
		return nil, err
	}
	return &Session{Path: path, store: s, w: bufio.NewWriter(f), f: f, header: rec.Header}, nil
}

// repairTornTail appends a newline when the file does not end in one.
func repairTornTail(f *os.File) error {
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return err
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], info.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	_, err = f.Write([]byte{'\n'})
	return err
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
	return s.AppendEntry(MsgEntry(m))
}

// AppendEntry writes one storage line (message or compaction marker)
// and fsyncs — every entry is durable before the call returns.
func (s *Session) AppendEntry(e Entry) error {
	var b []byte
	var err error
	if e.Compaction != nil {
		b, err = json.Marshal(e.Compaction)
	} else {
		b, err = json.Marshal(*e.Msg)
	}
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

// Record couples a loaded session with its stored entries.
type Record struct {
	Header  Header
	Entries []Entry
}

// Project computes the LLM context from the record's entries.
func (r *Record) Project() []llm.Message { return Project(r.Entries) }

// Transcript rebuilds the in-memory transcript from the projected
// context (leading system message first).
func (r *Record) Transcript() (*llm.Transcript, error) {
	msgs := r.Project()
	if len(msgs) == 0 {
		return nil, fmt.Errorf("session has no messages")
	}
	return llm.NewTranscript(msgs[0], msgs[1:]...)
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
		e, err := parseEntry(line)
		if err != nil {
			// Skip malformed lines (crash-torn fragments, future formats)
			// and keep loading — pi's loader does the same; a break here
			// would discard every valid entry after one bad line.
			continue
		}
		rec.Entries = append(rec.Entries, e)
	}
	if first {
		return nil, fmt.Errorf("empty session file %s", path)
	}
	return rec, sc.Err()
}

// parseEntry distinguishes compaction markers (top-level "kind":
// "compaction" — a field llm.Message never has) from message lines.
func parseEntry(line string) (Entry, error) {
	var probe struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(line), &probe); err != nil {
		return Entry{}, err
	}
	if probe.Kind == "compaction" {
		var c CompactionEntry
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return Entry{}, err
		}
		return Entry{Compaction: &c}, nil
	}
	var m llm.Message
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return Entry{}, err
	}
	if m.Role == "" {
		return Entry{}, fmt.Errorf("entry without role")
	}
	return Entry{Msg: &m}, nil
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
