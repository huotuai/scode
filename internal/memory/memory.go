// Package memory implements the assistant's long-term memory: durable
// facts extracted from conversations, stored per project, injected as
// a system-prompt section on the next session. The /config switches
// map onto it directly:
//
//   - autoMemory       — the whole subsystem's gate (no section, no
//     extraction when off)
//   - typedMemory      — memories carry a category (project/preference/
//     fact/task); off stores everything under "general"
//   - memoryRelevance  — on: only manually Selected entries are
//     injected; off: everything is injected and the model picks what
//     matters
//   - memoryAutoExtraction — on: memories are extracted when the
//     session closes; off: the /memory manager's manual trigger
//
// Storage is memory/<safe-cwd>.json under the config directory — the
// same scoping as the session store.
package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Categories (typed memory).
const (
	CatProject    = "project"    // 项目事实: 构建/架构/工具链
	CatPreference = "preference" // 用户偏好: 语言、风格、工作流习惯
	CatFact       = "fact"       // 关键技术事实与约束
	CatTask       = "task"       // 进行中/未完成事项
	CatGeneral    = "general"    // typed memory off
)

// CategoryLabel maps a category to its display label.
func CategoryLabel(cat string) string {
	switch cat {
	case CatProject:
		return "项目"
	case CatPreference:
		return "偏好"
	case CatFact:
		return "事实"
	case CatTask:
		return "任务"
	}
	return "通用"
}

// Categories lists the typed categories in display order.
var Categories = []string{CatProject, CatPreference, CatFact, CatTask}

// Entry is one remembered fact.
type Entry struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Content  string `json:"content"`
	// Selected marks entries the user hand-picked (the relevance
	// switch's manual curation).
	Selected bool   `json:"selected,omitempty"`
	Created  string `json:"created,omitempty"`
	Updated  string `json:"updated,omitempty"`
	Source   string `json:"source,omitempty"` // session id
}

// Op is one extraction operation the model returns.
type Op struct {
	Op       string `json:"op"` // add | update | delete
	ID       string `json:"id,omitempty"`
	Category string `json:"category,omitempty"`
	Content  string `json:"content,omitempty"`
}

// MaxEntries caps the stored list (extraction keeps under it; manual
// adds beyond it drop the oldest).
const MaxEntries = 40

// Store owns the memory file for one project.
type Store struct {
	Path string

	mu sync.Mutex
}

// safeCWD mirrors the session store's directory sanitization.
func safeCWD(cwd string) string {
	safe := strings.NewReplacer(":", "-", "\\", "-", "/", "-", " ", "_").Replace(filepath.Clean(cwd))
	safe = strings.TrimPrefix(strings.TrimPrefix(safe, "-"), ".")
	if safe == "" {
		safe = "default"
	}
	return safe
}

// StorePath is the memory file for a project.
func StorePath(cfgDir, cwd string) string {
	return filepath.Join(cfgDir, "memory", safeCWD(cwd)+".json")
}

// Open returns the store for a project (the file appears on first save).
func Open(cfgDir, cwd string) *Store {
	return &Store{Path: StorePath(cfgDir, cwd)}
}

type memoryFile struct {
	Memories []Entry `json:"memories"`
}

// Load reads the entries (empty slice when absent).
func (s *Store) Load() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func (s *Store) load() ([]Entry, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var f memoryFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("memory file: %w", err)
	}
	return f.Memories, nil
}

func (s *Store) save(entries []Entry) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(memoryFile{Memories: entries}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.Path, append(out, '\n'), 0o644)
}

// Apply commits extraction operations atomically and returns the
// counts. Unknown ids and malformed ops are skipped (the model is the
// author; the store is the gate).
func (s *Store) Apply(ops []Op, source string) (added, updated, deleted int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.load()
	if err != nil {
		return 0, 0, 0
	}
	now := time.Now().Format(time.RFC3339)
	byID := map[string]int{}
	for i, e := range entries {
		byID[e.ID] = i
	}
	for _, op := range ops {
		content := strings.TrimSpace(op.Content)
		switch op.Op {
		case "add":
			if content == "" {
				continue
			}
			cat := normalizeCategory(op.Category)
			// Dedupe: identical content under any category updates in place.
			dup := -1
			for i, e := range entries {
				if e.Content == content {
					dup = i
					break
				}
			}
			if dup >= 0 {
				entries[dup].Category = cat
				entries[dup].Updated = now
				updated++
				continue
			}
			if len(entries) >= MaxEntries {
				entries = entries[1:]
				byID = map[string]int{}
				for i, e := range entries {
					byID[e.ID] = i
				}
			}
			e := Entry{ID: newID(), Category: cat, Content: content, Created: now, Updated: now, Source: source}
			entries = append(entries, e)
			byID[e.ID] = len(entries) - 1
			added++
		case "update":
			if i, ok := byID[op.ID]; ok && content != "" {
				entries[i].Content = content
				if cat := normalizeCategory(op.Category); cat != CatGeneral {
					entries[i].Category = cat
				}
				entries[i].Updated = now
				updated++
			}
		case "delete":
			if i, ok := byID[op.ID]; ok {
				entries = append(entries[:i], entries[i+1:]...)
				byID = map[string]int{}
				for j, e := range entries {
					byID[e.ID] = j
				}
				deleted++
			}
		}
	}
	if added+updated+deleted == 0 {
		return 0, 0, 0
	}
	if err := s.save(entries); err != nil {
		return 0, 0, 0
	}
	return added, updated, deleted
}

// Delete removes one entry by id.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.load()
	if err != nil {
		return err
	}
	kept := entries[:0]
	for _, e := range entries {
		if e.ID != id {
			kept = append(kept, e)
		}
	}
	return s.save(kept)
}

// ToggleSelected flips one entry's hand-picked flag.
func (s *Store) ToggleSelected(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.load()
	if err != nil {
		return false, err
	}
	for i := range entries {
		if entries[i].ID == id {
			entries[i].Selected = !entries[i].Selected
			return entries[i].Selected, s.save(entries)
		}
	}
	return false, fmt.Errorf("memory %q not found", id)
}

func normalizeCategory(cat string) string {
	switch cat {
	case CatProject, CatPreference, CatFact, CatTask:
		return cat
	}
	return CatGeneral
}

// idSeq disambiguates ids minted within the same clock tick (Windows
// timer granularity makes nanosecond collisions routine).
var idSeq atomic.Int64

func newID() string {
	return fmt.Sprintf("m%s-%d", strconv.FormatInt(time.Now().UnixNano(), 36), idSeq.Add(1))
}

// FormatForPrompt renders the injected memory section: grouped by
// category when typed, only hand-picked entries when relevance
// selection is on, capped to the newest maxInject.
const maxInject = 60

func FormatForPrompt(entries []Entry, typed, relevanceOnly bool) string {
	if relevanceOnly {
		// A fresh slice — filtering in place (entries[:0]) would
		// overwrite the caller's backing array mid-loop.
		kept := make([]Entry, 0, len(entries))
		for _, e := range entries {
			if e.Selected {
				kept = append(kept, e)
			}
		}
		entries = kept
	}
	if len(entries) > maxInject {
		entries = entries[len(entries)-maxInject:]
	}
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Long-term memory for this project (durable facts from earlier sessions — use when relevant, ignore otherwise):")
	if typed {
		for _, cat := range Categories {
			var lines []string
			for _, e := range entries {
				if e.Category == cat {
					lines = append(lines, "- "+e.Content)
				}
			}
			if len(lines) > 0 {
				fmt.Fprintf(&b, "\n%s:\n%s", CategoryLabel(cat), strings.Join(lines, "\n"))
			}
		}
		// Unclassified entries still ride along.
		var lines []string
		for _, e := range entries {
			if e.Category == CatGeneral {
				lines = append(lines, "- "+e.Content)
			}
		}
		if len(lines) > 0 {
			fmt.Fprintf(&b, "\n通用:\n%s", strings.Join(lines, "\n"))
		}
		return b.String()
	}
	for _, e := range entries {
		b.WriteString("\n- " + e.Content)
	}
	return b.String()
}

// SortEntries orders entries for display (category order, then newest
// first).
func SortEntries(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		ci, cj := categoryOrder(entries[i].Category), categoryOrder(entries[j].Category)
		if ci != cj {
			return ci < cj
		}
		return entries[i].Updated > entries[j].Updated
	})
}

func categoryOrder(cat string) int {
	for i, c := range Categories {
		if c == cat {
			return i
		}
	}
	return len(Categories)
}
