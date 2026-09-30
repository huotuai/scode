// Package session persists transcripts as append-only JSONL files in
// pi's session format: a header line carrying identity, then one entry
// per line. Entries form a TREE via id/parentId links (pi's session
// v2); the conversation used for context is the PATH from the leaf
// back to the root. Files are never rewritten during a conversation —
// crash safety and cache stability fall out of the same discipline.
package session

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"scode/internal/llm"
)

const formatVersion = 2

// Header is the first line of a session file (pi's SessionHeader).
type Header struct {
	Type          string `json:"type"` // "session"
	Version       int    `json:"version"`
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"` // ISO 8601
	CWD           string `json:"cwd"`
	ParentSession string `json:"parentSession,omitempty"`
}

// Entry is one storage line (pi's SessionEntryBase): a message or a
// compaction marker, linked into the session tree by ID/ParentID.
// Unknown pi entry types (model_change, label, custom, ...) load as
// bare Entries: they participate in the tree but not in projection.
type Entry struct {
	Type      string `json:"type"` // "message" | "compaction" | other pi types
	ID        string `json:"id"`
	ParentID  string `json:"parentId"`
	Timestamp string `json:"timestamp"` // ISO 8601

	Msg        *llm.Message     // set when Type == "message"
	Compaction *CompactionEntry // set when Type == "compaction"
	Mode       *ModeEntry       // set when Type == "mode"
	Model      *ModelEntry      // set when Type == "model"
	Sandbox    *SandboxEntry    // set when Type == "sandbox"
	Title      *TitleEntry      // set when Type == "title"
	Plan       *PlanEntry       // set when Type == "plan"
	Thinking   *ThinkingEntry   // set when Type == "thinking"
}

// ModeEntry records a collaboration-mode switch (dsh's plan/mode event:
// log-only, whole-value replace — the last one wins; replay restores the
// mode on resume/fork). Mode is "plan" or "default".
type ModeEntry struct {
	Mode string `json:"mode"`
}

// ModelEntry records a model switch (pi's model_change entry: log-only,
// whole-value replace — the last one wins; replay restores the model on
// resume/fork).
type ModelEntry struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// CurrentMode folds the mode entries (last wins); "" means never set
// (default mode).
func CurrentMode(entries []Entry) string {
	mode := ""
	for _, e := range entries {
		if e.Mode != nil {
			mode = e.Mode.Mode
		}
	}
	return mode
}

// SandboxEntry records a sandbox-mode switch (dsh's sandbox/mode event:
// log-only, whole-value replace — the last one wins; replay restores the
// mode on resume/fork).
type SandboxEntry struct {
	Mode string `json:"mode"`
}

// CurrentSandbox folds the sandbox entries (last wins); "" means never set
// (the settings default applies).
func CurrentSandbox(entries []Entry) string {
	mode := ""
	for _, e := range entries {
		if e.Sandbox != nil {
			mode = e.Sandbox.Mode
		}
	}
	return mode
}

// TitleEntry records an auto-generated session title (an LLM summary of
// the first user message: log-only, whole-value replace — the last one
// wins; replay restores it on resume/fork). Clients that cannot derive a
// title fall back to the first user message.
type TitleEntry struct {
	Title string `json:"title"`
}

// CurrentTitle folds the title entries (last wins); "" means untitled.
func CurrentTitle(entries []Entry) string {
	title := ""
	for _, e := range entries {
		if e.Title != nil {
			title = e.Title.Title
		}
	}
	return title
}

// PlanItem is one step of the agent's working plan. Status is
// "pending" | "in_progress" | "completed".
type PlanItem struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

// PlanEntry records the agent's plan progress (the update_plan tool's
// whole list: log-only, whole-value replace — the last one wins; replay
// restores the plan on resume/fork). The desktop polls it via
// session/plan to render progress.
type PlanEntry struct {
	Explanation string     `json:"explanation,omitempty"`
	Items       []PlanItem `json:"items"`
}

// CurrentPlan folds the plan entries (last wins); nil means the plan
// was never set.
func CurrentPlan(entries []Entry) *PlanEntry {
	var plan *PlanEntry
	for _, e := range entries {
		if e.Plan != nil {
			plan = e.Plan
		}
	}
	return plan
}

// CurrentModel folds the model entries (last wins); empty values mean
// the model was never switched.
func CurrentModel(entries []Entry) (provider, model string) {
	for _, e := range entries {
		if e.Model != nil {
			provider, model = e.Model.Provider, e.Model.Model
		}
	}
	return provider, model
}

// ThinkingEntry records a reasoning-effort switch (log-only,
// whole-value replace — the last one wins; replay restores the level
// on resume/fork). Level is "off" | "low" | "medium" | "high".
type ThinkingEntry struct {
	Level string `json:"level"`
}

// CurrentThinking folds the thinking entries (last wins); "" means
// never set (the settings default applies).
func CurrentThinking(entries []Entry) string {
	level := ""
	for _, e := range entries {
		if e.Thinking != nil {
			level = e.Thinking.Level
		}
	}
	return level
}

// MsgEntry wraps a message as an entry (ID/ParentID/Timestamp are
// stamped on append).
func MsgEntry(m llm.Message) Entry { return Entry{Type: "message", Msg: &m} }

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

// PruneOlderThan deletes session files whose last modification is more
// than days old (the /config retention period); paths in keep survive
// (the live session). days <= 0 prunes nothing. Returns the number of
// files removed.
func (s *Store) PruneOlderThan(days int, keep ...string) int {
	if days <= 0 {
		return 0
	}
	cutoff := s.Now().AddDate(0, 0, -days)
	survive := make(map[string]bool, len(keep))
	for _, k := range keep {
		survive[k] = true
	}
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		p := filepath.Join(s.Root, e.Name())
		if survive[p] {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			if os.Remove(p) == nil {
				removed++
			}
		}
	}
	return removed
}

// NewID generates a unique short id (pi's generateId): 8 hex chars,
// collision-checked against existing session files; falls back to a
// full-length hex id if collisions somehow persist.
func (s *Store) NewID() string {
	for i := 0; i < 100; i++ {
		id := randomHex(4) // 8 hex chars
		if _, err := os.Stat(filepath.Join(s.Root, id+".jsonl")); os.IsNotExist(err) {
			return id
		}
	}
	return randomHex(16)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is not survivable for id generation.
		panic(err)
	}
	return hex.EncodeToString(b)
}

// iso renders t in pi's timestamp format.
func iso(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// Create starts a new session file with a header.
func (s *Store) Create(id, cwd string) (*Session, error) {
	if id == "" {
		id = s.NewID()
	}
	h := Header{
		Type: "session", Version: formatVersion, ID: id,
		Timestamp: iso(s.Now()), CWD: cwd,
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
	return &Session{Path: path, store: s, f: f, header: h}, nil
}

// Delete removes the session file. A missing file is not an error:
// delete is idempotent from the caller's perspective.
func (s *Store) Delete(id string) error {
	if id == "" || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("invalid session id %q", id)
	}
	err := os.Remove(filepath.Join(s.Root, id+".jsonl"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// HasMessages reports whether the session holds any conversation
// entries. A freshly created session has only header/model/mode lines;
// it becomes non-empty once the first message lands. Both storage
// shapes are recognized: v2 envelopes ("type":"message") and legacy v1
// bare message lines — the "role" key only ever appears on a message,
// and JSON string escaping keeps either literal from occurring inside a
// string value, so a v1 session with real conversation is never
// mistaken for an empty one (the prune sweep deletes on this verdict).
func (s *Store) HasMessages(id string) (bool, error) {
	if id == "" || strings.ContainsAny(id, `/\`) {
		return false, fmt.Errorf("invalid session id %q", id)
	}
	b, err := os.ReadFile(filepath.Join(s.Root, id+".jsonl"))
	if err != nil {
		return false, err
	}
	return bytes.Contains(b, []byte(`"type":"message"`)) ||
		bytes.Contains(b, []byte(`"role":`)), nil
}

// Fork clones an existing session's PATH into a NEW file (pi's
// createBranchedSession, clone form): the clone's header links back
// via parentSession, entry ids/parents are regenerated as a fresh
// linear chain, and compaction kept-range pointers are remapped to the
// new ids. The original stays untouched. When upto > 0 only the first
// upto path entries are copied.
func (s *Store) Fork(id string, upto int) (string, error) {
	rec, err := s.Load(id)
	if err != nil {
		return "", err
	}
	path := rec.Path()
	if upto > 0 && upto < len(path) {
		path = path[:upto]
	}
	sess, err := s.Create("", rec.Header.CWD)
	if err != nil {
		return "", err
	}
	sess.header.ParentSession = rec.Path0() // link back to the source file
	// Rewrite the header with parentSession set (file is header-only).
	if err := sess.rewriteHeader(); err != nil {
		sess.Close() //nolint:errcheck
		return "", err
	}
	// index of firstKeptEntryId within the source path, for remapping.
	idxOf := map[string]int{}
	for i, e := range path {
		idxOf[e.ID] = i
	}
	newIDs := make([]string, len(path))
	for i, e := range path {
		if e.Compaction != nil && e.Compaction.FirstKeptEntryID != "" {
			if j, ok := idxOf[e.Compaction.FirstKeptEntryID]; ok {
				e.Compaction.FirstKeptEntryID = newIDs[j]
			}
		}
		// Fresh chain in the clone: the append stamps new ids/parents.
		e.ID, e.ParentID, e.Timestamp = "", "", ""
		stamped, err := sess.AppendEntry(e)
		if err != nil {
			sess.Close() //nolint:errcheck
			return "", err
		}
		newIDs[i] = stamped.ID
	}
	if err := sess.Close(); err != nil {
		return "", err
	}
	return sess.header.ID, nil
}

// rewriteHeader truncates a header-only session file and rewrites the
// header line (used by Fork to set parentSession before any entries).
func (s *Session) rewriteHeader() error {
	if _, err := s.f.Seek(0, 0); err != nil {
		return err
	}
	if err := s.f.Truncate(0); err != nil {
		return err
	}
	b, _ := json.Marshal(s.header)
	if _, err := s.f.Write(append(b, '\n')); err != nil {
		return err
	}
	return s.f.Sync()
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
	return &Session{Path: path, store: s, f: f, header: rec.Header, leafID: rec.leafID}, nil
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
	f      *os.File
	header Header
	leafID string // id of the last entry on the current branch
}

// Append writes one message line and flushes.
func (s *Session) Append(m llm.Message) (Entry, error) {
	return s.AppendEntry(MsgEntry(m))
}

// AppendEntry stamps the entry with a fresh id and pi's ISO timestamp,
// writes it and fsyncs — every entry is durable before the call
// returns. The parent defaults to the current leaf (linear append); a
// caller-supplied ParentID is respected, which is how branches form
// (pi's tree: appending at an older entry forks the path). The
// stamped entry (with ID) is returned, and the leaf advances to it.
func (s *Session) AppendEntry(e Entry) (Entry, error) {
	e.ID = randomHex(4)
	if e.ParentID == "" {
		e.ParentID = s.leafID
	}
	e.Timestamp = iso(s.store.Now())
	var b []byte
	var err error
	switch {
	case e.Mode != nil:
		e.Type = "mode"
		b, err = json.Marshal(struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			ParentID  string `json:"parentId"`
			Timestamp string `json:"timestamp"`
			*ModeEntry
		}{e.Type, e.ID, e.ParentID, e.Timestamp, e.Mode})
	case e.Model != nil:
		e.Type = "model"
		b, err = json.Marshal(struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			ParentID  string `json:"parentId"`
			Timestamp string `json:"timestamp"`
			*ModelEntry
		}{e.Type, e.ID, e.ParentID, e.Timestamp, e.Model})
	case e.Sandbox != nil:
		e.Type = "sandbox"
		b, err = json.Marshal(struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			ParentID  string `json:"parentId"`
			Timestamp string `json:"timestamp"`
			*SandboxEntry
		}{e.Type, e.ID, e.ParentID, e.Timestamp, e.Sandbox})
	case e.Title != nil:
		e.Type = "title"
		b, err = json.Marshal(struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			ParentID  string `json:"parentId"`
			Timestamp string `json:"timestamp"`
			*TitleEntry
		}{e.Type, e.ID, e.ParentID, e.Timestamp, e.Title})
	case e.Plan != nil:
		e.Type = "plan"
		b, err = json.Marshal(struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			ParentID  string `json:"parentId"`
			Timestamp string `json:"timestamp"`
			*PlanEntry
		}{e.Type, e.ID, e.ParentID, e.Timestamp, e.Plan})
	case e.Thinking != nil:
		e.Type = "thinking"
		b, err = json.Marshal(struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			ParentID  string `json:"parentId"`
			Timestamp string `json:"timestamp"`
			*ThinkingEntry
		}{e.Type, e.ID, e.ParentID, e.Timestamp, e.Thinking})
	case e.Compaction != nil:
		e.Type = "compaction"
		b, err = json.Marshal(struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			ParentID  string `json:"parentId"`
			Timestamp string `json:"timestamp"`
			*CompactionEntry
		}{e.Type, e.ID, e.ParentID, e.Timestamp, e.Compaction})
	case e.Msg != nil:
		e.Type = "message"
		b, err = json.Marshal(struct {
			Type      string       `json:"type"`
			ID        string       `json:"id"`
			ParentID  string       `json:"parentId"`
			Timestamp string       `json:"timestamp"`
			Message   *llm.Message `json:"message"`
		}{e.Type, e.ID, e.ParentID, e.Timestamp, e.Msg})
	default:
		err = fmt.Errorf("entry has neither message nor compaction")
	}
	if err != nil {
		return Entry{}, err
	}
	if err := s.writeEntry(b); err != nil {
		return Entry{}, err
	}
	s.leafID = e.ID
	return e, nil
}

// writeEntry appends one encoded line and fsyncs it. Writes go straight to
// the file: the previous shared bufio.Writer kept its FIRST error forever, so
// one transient failure poisoned every later append until the session was
// reopened. Windows sharing violations (antivirus / backup / indexer briefly
// holding the file) are retried a few times before giving up.
func (s *Session) writeEntry(b []byte) error {
	line := append(b, '\n')
	for attempt := 0; ; attempt++ {
		n, err := s.f.Write(line)
		if err == nil && n < len(line) {
			err = io.ErrShortWrite
		}
		if err == nil {
			if err = s.f.Sync(); err == nil { // crash safety: every entry is durable
				return nil
			}
		}
		// A partial write cannot be retried safely (appending again would
		// duplicate the prefix); fail fast.
		if n > 0 && n < len(line) {
			return err
		}
		if attempt >= 4 || !transientWriteError(err) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 25 * time.Millisecond)
	}
}

// transientWriteError reports IO errors worth retrying: the file was briefly
// held by another process (antivirus, backup, search indexer).
func transientWriteError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	if runtime.GOOS == "windows" {
		var errno syscall.Errno
		if errors.As(err, &errno) {
			switch uintptr(errno) {
			case 5, 32, 33: // ACCESS_DENIED, SHARING_VIOLATION, LOCK_VIOLATION
				return true
			}
		}
	}
	return false
}

// AppendAll writes several messages in order.
func (s *Session) AppendAll(msgs []llm.Message) error {
	for _, m := range msgs {
		if _, err := s.Append(m); err != nil {
			return err
		}
	}
	return nil
}

// Close closes the file (every append already flushed and fsynced).
func (s *Session) Close() error {
	return s.f.Close()
}

// Header returns the session's header.
func (s *Session) Header() Header { return s.header }

// Record couples a loaded session with its entries: Raw is the file in
// order; the PATH (tree walk from the leaf) is the conversation.
type Record struct {
	Header Header
	Raw    []Entry
	byID   map[string]Entry
	leafID string
	root   string
}

// Path walks the tree from the leaf back to the root (pi's
// buildSessionPath). Linear files get file order.
func (r *Record) Path() []Entry {
	if len(r.Raw) == 0 {
		return nil
	}
	leaf, ok := r.byID[r.leafID]
	if !ok {
		leaf = r.Raw[len(r.Raw)-1]
	}
	var path []Entry
	cur := leaf
	for {
		path = append(path, cur)
		if cur.ParentID == "" {
			break
		}
		next, ok := r.byID[cur.ParentID]
		if !ok {
			break
		}
		cur = next
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

// Path0 is the session file path (helper for parentSession links).
func (r *Record) Path0() string { return filepath.Join(r.root, r.Header.ID+".jsonl") }

// Project computes the LLM conversation from the record's PATH
// (system messages excluded — they are rebuilt at load, never stored).
func (r *Record) Project() []llm.Message { return Project(r.Path()) }

// Transcript rebuilds the in-memory transcript: the freshly built
// system message first, then the projected conversation.
func (r *Record) Transcript(sys llm.Message) (*llm.Transcript, error) {
	return llm.NewTranscript(sys, r.Project()...)
}

// LatestFileOps recovers the working set from the newest compaction
// marker on the PATH.
func (r *Record) LatestFileOps() FileOps {
	path := r.Path()
	for i := len(path) - 1; i >= 0; i-- {
		if c := path[i].Compaction; c != nil {
			var ops FileOps
			if c.Details != nil {
				ops = FileOps{Read: c.Details.ReadFiles, Modified: c.Details.ModifiedFiles}
			}
			return ops
		}
	}
	return FileOps{}
}

// root is set on Load so Path0 can resolve the file location.
func (r *Record) withRoot(root string) *Record { r.root = root; return r }

// Load reads a session file (used by --resume), migrating the legacy
// v1 format (bare message lines, kind-tagged markers and header) into
// the tree shape on read — pi's migrateV1ToV2 does the same.
func (s *Store) Load(id string) (*Record, error) {
	path := filepath.Join(s.Root, id+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 32<<20)

	rec := &Record{byID: map[string]Entry{}, root: s.Root}
	first := true
	legacy := false
	prevID := ""
	n := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if first {
			first = false
			h, isLegacy, err := parseHeader(line)
			if err != nil {
				return nil, fmt.Errorf("header: %w", err)
			}
			rec.Header = h
			legacy = isLegacy
			continue
		}
		e, err := parseEntry(line)
		if err != nil {
			// Skip malformed lines (crash-torn fragments, future formats)
			// and keep loading — pi's loader does the same; a break here
			// would discard every valid entry after one bad line.
			continue
		}
		if legacy || e.ID == "" {
			// Migration: assign ids and linear parents in file order.
			n++
			e.ID = fmt.Sprintf("mig-%06d", n)
			e.ParentID = prevID
			if e.Timestamp == "" {
				e.Timestamp = iso(time.UnixMilli(0))
			}
			if e.Compaction != nil {
				migrateLegacyMarker(e.Compaction, n)
			}
		}
		prevID = e.ID
		rec.Raw = append(rec.Raw, e)
		rec.byID[e.ID] = e
		rec.leafID = e.ID
	}
	if first {
		return nil, fmt.Errorf("empty session file %s", path)
	}
	return rec, sc.Err()
}

// parseHeader detects the header shape: pi v2 (type "session") or the
// legacy scode v1 (kind "header", integer v, epoch CreatedAt).
func parseHeader(line string) (Header, bool, error) {
	var probe struct {
		Type string `json:"type"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(line), &probe); err != nil {
		return Header{}, false, err
	}
	if probe.Type == "session" {
		var h Header
		if err := json.Unmarshal([]byte(line), &h); err != nil {
			return Header{}, false, err
		}
		return h, false, nil
	}
	if probe.Kind == "header" {
		var old struct {
			ID        string `json:"id"`
			CreatedAt int64  `json:"createdAt"`
			CWD       string `json:"cwd"`
		}
		if err := json.Unmarshal([]byte(line), &old); err != nil {
			return Header{}, false, err
		}
		return Header{
			Type: "session", Version: 1, ID: old.ID,
			Timestamp: iso(time.UnixMilli(old.CreatedAt)), CWD: old.CWD,
		}, true, nil
	}
	return Header{}, false, fmt.Errorf("not a session header")
}

// parseEntry distinguishes entry shapes: pi v2 envelopes (type
// "message"/"compaction", or other pi types kept as tree nodes),
// legacy compaction markers (kind "compaction"), and legacy bare
// message lines.
func parseEntry(line string) (Entry, error) {
	var probe struct {
		Type string `json:"type"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(line), &probe); err != nil {
		return Entry{}, err
	}
	switch {
	case probe.Type == "message":
		var env struct {
			ID        string       `json:"id"`
			ParentID  string       `json:"parentId"`
			Timestamp string       `json:"timestamp"`
			Message   *llm.Message `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			return Entry{}, err
		}
		if env.Message == nil || env.Message.Role == "" {
			return Entry{}, fmt.Errorf("message entry without role")
		}
		return Entry{Type: "message", ID: env.ID, ParentID: env.ParentID, Timestamp: env.Timestamp, Msg: env.Message}, nil
	case probe.Type == "compaction":
		var env struct {
			ID        string `json:"id"`
			ParentID  string `json:"parentId"`
			Timestamp string `json:"timestamp"`
			CompactionEntry
		}
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			return Entry{}, err
		}
		c := env.CompactionEntry
		return Entry{Type: "compaction", ID: env.ID, ParentID: env.ParentID, Timestamp: env.Timestamp, Compaction: &c}, nil
	case probe.Type == "mode":
		var env struct {
			ID        string `json:"id"`
			ParentID  string `json:"parentId"`
			Timestamp string `json:"timestamp"`
			ModeEntry
		}
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			return Entry{}, err
		}
		m := env.ModeEntry
		return Entry{Type: "mode", ID: env.ID, ParentID: env.ParentID, Timestamp: env.Timestamp, Mode: &m}, nil
	case probe.Type == "model":
		var env struct {
			ID        string `json:"id"`
			ParentID  string `json:"parentId"`
			Timestamp string `json:"timestamp"`
			ModelEntry
		}
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			return Entry{}, err
		}
		m := env.ModelEntry
		return Entry{Type: "model", ID: env.ID, ParentID: env.ParentID, Timestamp: env.Timestamp, Model: &m}, nil
	case probe.Type == "sandbox":
		var env struct {
			ID        string `json:"id"`
			ParentID  string `json:"parentId"`
			Timestamp string `json:"timestamp"`
			SandboxEntry
		}
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			return Entry{}, err
		}
		s := env.SandboxEntry
		return Entry{Type: "sandbox", ID: env.ID, ParentID: env.ParentID, Timestamp: env.Timestamp, Sandbox: &s}, nil
	case probe.Type == "title":
		var env struct {
			ID        string `json:"id"`
			ParentID  string `json:"parentId"`
			Timestamp string `json:"timestamp"`
			TitleEntry
		}
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			return Entry{}, err
		}
		t := env.TitleEntry
		return Entry{Type: "title", ID: env.ID, ParentID: env.ParentID, Timestamp: env.Timestamp, Title: &t}, nil
	case probe.Type == "plan":
		var env struct {
			ID        string `json:"id"`
			ParentID  string `json:"parentId"`
			Timestamp string `json:"timestamp"`
			PlanEntry
		}
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			return Entry{}, err
		}
		p := env.PlanEntry
		return Entry{Type: "plan", ID: env.ID, ParentID: env.ParentID, Timestamp: env.Timestamp, Plan: &p}, nil
	case probe.Type == "thinking":
		var env struct {
			ID        string `json:"id"`
			ParentID  string `json:"parentId"`
			Timestamp string `json:"timestamp"`
			ThinkingEntry
		}
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			return Entry{}, err
		}
		t := env.ThinkingEntry
		return Entry{Type: "thinking", ID: env.ID, ParentID: env.ParentID, Timestamp: env.Timestamp, Thinking: &t}, nil
	case probe.Type != "":
		// Another pi entry type: keep the tree links, drop the payload.
		var env struct {
			ID        string `json:"id"`
			ParentID  string `json:"parentId"`
			Timestamp string `json:"timestamp"`
		}
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			return Entry{}, err
		}
		return Entry{Type: probe.Type, ID: env.ID, ParentID: env.ParentID, Timestamp: env.Timestamp}, nil
	case probe.Kind == "compaction":
		c, err := parseLegacyCompaction(line)
		if err != nil {
			return Entry{}, err
		}
		return Entry{Type: "compaction", Compaction: &c}, nil
	default:
		var m llm.Message
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			return Entry{}, err
		}
		if m.Role == "" {
			return Entry{}, fmt.Errorf("entry without role")
		}
		return Entry{Type: "message", Msg: &m}, nil
	}
}

// parseLegacyCompaction reads a v1 kind-tagged compaction marker. v1
// carried the working set as flat top-level readFiles/modifiedFiles
// fields; they fold into Details (the v2/pi shape) so the carried
// working set survives the migration instead of silently dropping.
func parseLegacyCompaction(line string) (CompactionEntry, error) {
	var c struct {
		CompactionEntry
		ReadFiles     []string `json:"readFiles"`
		ModifiedFiles []string `json:"modifiedFiles"`
	}
	if err := json.Unmarshal([]byte(line), &c); err != nil {
		return CompactionEntry{}, err
	}
	out := c.CompactionEntry
	if out.Details == nil && (len(c.ReadFiles) > 0 || len(c.ModifiedFiles) > 0) {
		out.Details = &CompactionDetails{ReadFiles: c.ReadFiles, ModifiedFiles: c.ModifiedFiles}
	}
	return out, nil
}

// migrateLegacyMarker converts a legacy index-based kept range to the
// id-based form. n is the 1-based position of the marker's own entry,
// so a kept index pointing past it clamps to "keep nothing".
func migrateLegacyMarker(c *CompactionEntry, n int) {
	if c.FirstKeptIndex == nil {
		return
	}
	idx := *c.FirstKeptIndex
	if idx >= n {
		c.FirstKeptEntryID = ""
	} else {
		c.FirstKeptEntryID = fmt.Sprintf("mig-%06d", idx+1)
	}
	c.FirstKeptIndex = nil
}

// List returns available session ids, most recently modified first
// (pi sorts by last activity).
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type sessFile struct {
		id    string
		mtime time.Time
	}
	var files []sessFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, sessFile{id: strings.TrimSuffix(e.Name(), ".jsonl"), mtime: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.After(files[j].mtime) })
	ids := make([]string, 0, len(files))
	for _, f := range files {
		ids = append(ids, f.id)
	}
	return ids, nil
}
