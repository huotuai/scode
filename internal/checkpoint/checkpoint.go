// Package checkpoint implements code rewind: the BEFORE state of
// every file an AI edit touches is snapshotted into per-session
// checkpoint storage, and /rewind restores any of them. The gate is
// the /config rewindCheckpoints switch (default on) — off means no
// snapshots are taken.
//
// Storage layout under <cfgDir>/checkpoints/<session>/:
//
//	manifest.json        the ordered checkpoint list (newest last)
//	c<seq>/manifest ...  one directory per checkpoint holding the
//	                     captured file blobs (path-keyed)
package checkpoint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// KeepPerSession caps the checkpoints kept per session (oldest pruned).
const KeepPerSession = 50

// MaxSnapshotBytes caps what gets snapshotted: a file larger than this
// is recorded as TooLarge (no blob) — restoring skips it, the listings
// say so. Snapshotting a 500MB log would cost seconds and 500MB of
// disk per point, and the editor tools cannot meaningfully process
// such files anyway.
const MaxSnapshotBytes = 10 << 20

// FileSnap records one captured file. Existed=false means the edit
// CREATED the file — restoring deletes it again. TooLarge means the
// file exceeded MaxSnapshotBytes: the edit point is recorded, but
// there is no blob to restore from.
type FileSnap struct {
	Path     string `json:"path"` // absolute
	Existed  bool   `json:"existed"`
	TooLarge bool   `json:"tooLarge,omitempty"`
}

// Checkpoint is one rewind point.
type Checkpoint struct {
	ID      string     `json:"id"` // c001, c002, …
	Time    time.Time  `json:"time"`
	Tool    string     `json:"tool"`    // edit | write
	Summary string     `json:"summary"` // the target path, short form
	Files   []FileSnap `json:"files"`
}

// Store owns the checkpoint tree.
type Store struct {
	Root string

	mu sync.Mutex
}

// NewStore builds the store root under the config directory.
func NewStore(cfgDir string) *Store {
	return &Store{Root: filepath.Join(cfgDir, "checkpoints")}
}

func (s *Store) sessionDir(sessionID string) string {
	return filepath.Join(s.Root, sessionID)
}

// safeName encodes an absolute path into one blob filename (the path
// may itself contain separators and colons).
func safeName(p string) string {
	r := strings.NewReplacer(":", "-", "\\", "-", "/", "-", " ", "_", ".", "-")
	n := r.Replace(strings.ToLower(p))
	if n == "" {
		n = "f"
	}
	return n
}

type manifestFile struct {
	NextSeq int          `json:"nextSeq"`
	Items   []Checkpoint `json:"items"`
}

func (s *Store) loadManifest(sessionID string) manifestFile {
	var m manifestFile
	data, err := os.ReadFile(filepath.Join(s.sessionDir(sessionID), "manifest.json"))
	if err == nil {
		json.Unmarshal(data, &m) //nolint:errcheck — a corrupt manifest restarts the seq
	}
	return m
}

func (s *Store) saveManifest(sessionID string, m manifestFile) error {
	dir := s.sessionDir(sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), append(out, '\n'), 0o644)
}

// Capture snapshots the current content of paths (missing files record
// Existed=false) and appends the checkpoint. Best-effort per file: an
// unreadable file records nothing for itself rather than failing the
// whole capture.
func (s *Store) Capture(sessionID, tool, summary string, paths []string) (*Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.loadManifest(sessionID)
	id := fmt.Sprintf("c%03d", m.NextSeq+1)
	cp := Checkpoint{ID: id, Time: time.Now(), Tool: tool, Summary: summary}
	dir := filepath.Join(s.sessionDir(sessionID), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				cp.Files = append(cp.Files, FileSnap{Path: p, Existed: false})
				continue
			}
			continue // unreadable: no snapshot for this one
		}
		if info.Size() > MaxSnapshotBytes {
			// Size guard: record the point, skip the blob — a giant file
			// would cost seconds to copy and gigabytes across the cap.
			cp.Files = append(cp.Files, FileSnap{Path: p, Existed: true, TooLarge: true})
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue // unreadable: no snapshot for this one
		}
		blob := filepath.Join(dir, safeName(p))
		if err := os.WriteFile(blob, data, 0o644); err != nil {
			continue
		}
		cp.Files = append(cp.Files, FileSnap{Path: p, Existed: true})
	}
	if len(cp.Files) == 0 {
		os.RemoveAll(dir) //nolint:errcheck — nothing captured
		return nil, fmt.Errorf("no snapshotable files among %v", paths)
	}
	m.NextSeq++
	m.Items = append(m.Items, cp)
	// Prune before persisting so the manifest and the tree stay in step.
	if len(m.Items) > KeepPerSession {
		drop := m.Items[:len(m.Items)-KeepPerSession]
		m.Items = m.Items[len(m.Items)-KeepPerSession:]
		for _, old := range drop {
			os.RemoveAll(filepath.Join(s.sessionDir(sessionID), old.ID)) //nolint:errcheck
		}
	}
	if err := s.saveManifest(sessionID, m); err != nil {
		return nil, err
	}
	return &cp, nil
}

// Remove drops a checkpoint (used when the wrapped tool call FAILED —
// nothing changed, the point is meaningless).
func (s *Store) Remove(sessionID, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.loadManifest(sessionID)
	kept := m.Items[:0]
	for _, cp := range m.Items {
		if cp.ID != id {
			kept = append(kept, cp)
		}
	}
	m.Items = kept
	os.RemoveAll(filepath.Join(s.sessionDir(sessionID), id)) //nolint:errcheck
	s.saveManifest(sessionID, m)                             //nolint:errcheck
}

// List returns the session's checkpoints, NEWEST FIRST.
func (s *Store) List(sessionID string) []Checkpoint {
	m := s.loadManifest(sessionID)
	out := make([]Checkpoint, len(m.Items))
	copy(out, m.Items)
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out
}

// Restore rolls files back to the checkpoint's captured state and
// returns how many files it touched.
func (s *Store) Restore(sessionID, id string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.loadManifest(sessionID)
	var cp *Checkpoint
	for i := range m.Items {
		if m.Items[i].ID == id {
			cp = &m.Items[i]
			break
		}
	}
	if cp == nil {
		return 0, fmt.Errorf("checkpoint %q not found", id)
	}
	n := 0
	for _, f := range cp.Files {
		if f.TooLarge {
			continue // never archived: nothing to restore from
		}
		blob := filepath.Join(s.sessionDir(sessionID), id, safeName(f.Path))
		if !f.Existed {
			// The edit created the file: restore deletes it.
			if _, err := os.Stat(f.Path); err == nil {
				if os.Remove(f.Path) == nil {
					n++
				}
			}
			continue
		}
		data, err := os.ReadFile(blob)
		if err != nil {
			return n, fmt.Errorf("snapshot for %s missing: %w", f.Path, err)
		}
		if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
			return n, err
		}
		if err := os.WriteFile(f.Path, data, 0o644); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
