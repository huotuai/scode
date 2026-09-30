// Package subagent implements isolated sub-agents: self-contained
// research runs the main model delegates to through the task tool.
// Every run gets a FRESH agent loop, tool registry, and transcript —
// sub-agents never see the parent conversation, never mutate session
// state, and never share anything with each other (parallel task calls
// are fully independent). Definitions live in agents.json at the
// config directory:
//
//	{"agents": [
//	  {"name": "researcher", "description": "代码考古与调研", "model": "openai-compat:gpt-5", "effort": "high"}
//	]}
//
// model is "provider:model" ("" inherits the session model); effort is
// "" | off | low | medium | high.
package subagent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Spec is one sub-agent definition.
type Spec struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Model       string `json:"model,omitempty"`
	Effort      string `json:"effort,omitempty"`
}

// ValidEffort reports whether s.Effort is a legal reasoning effort.
func (s Spec) ValidEffort() bool {
	switch s.Effort {
	case "", "off", "low", "medium", "high":
		return true
	}
	return false
}

// Validate checks the invariants every saved spec must hold.
func (s Spec) Validate() error {
	s.Name = strings.TrimSpace(s.Name)
	if s.Name == "" {
		return fmt.Errorf("agent name is required")
	}
	if strings.ContainsAny(s.Name, " \t\n\"") {
		return fmt.Errorf("agent name must not contain spaces or quotes")
	}
	if !s.ValidEffort() {
		return fmt.Errorf("effort %q invalid (off|low|medium|high, empty = session default)", s.Effort)
	}
	return nil
}

type agentsFile struct {
	Agents []Spec `json:"agents"`
}

// BuiltinSpecs are the shipped delegates: always available with no
// configuration. An agents.json entry with the same name OVERRIDES one
// (a custom model or effort) without deleting the original.
func BuiltinSpecs() []Spec {
	return []Spec{
		{
			Name:        "general-purpose",
			Description: "General-purpose agent for researching complex questions, searching for code, and executing multi-step tasks.",
		},
		{
			Name:        "Explore",
			Description: "Read-only search agent for broad fan-out searches: locate code, map structure, gather evidence across many files.",
		},
	}
}

// All merges the built-ins with the user's definitions; same-name file
// entries win. The result is sorted by name and is the effective
// delegation surface (the tool's unknown-agent listing, the /agents
// views).
func All(user []Spec) []Spec {
	out := append([]Spec{}, BuiltinSpecs()...)
	for _, s := range user {
		replaced := false
		for i := range out {
			if out[i].Name == s.Name {
				out[i] = s
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// IsBuiltin reports whether name refers to a shipped delegate.
func IsBuiltin(name string) bool {
	for _, s := range BuiltinSpecs() {
		if s.Name == name {
			return true
		}
	}
	return false
}

// Load reads the agent definitions (empty slice when the file is
// absent); unknown keys in the document are preserved by the RMW pair.
func Load(path string) ([]Spec, error) {
	doc := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("agents.json: %w", err)
	}
	raw, _ := json.Marshal(doc["agents"])
	var specs []Spec
	if err := json.Unmarshal(raw, &specs); err != nil {
		return nil, fmt.Errorf("agents.json: agents: %w", err)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs, nil
}

// Upsert inserts or replaces one definition by name (map-level RMW:
// unknown document keys survive).
func Upsert(path string, s Spec) error {
	if err := s.Validate(); err != nil {
		return err
	}
	doc := map[string]any{}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("agents.json: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	raw, _ := json.Marshal(doc["agents"])
	var specs []Spec
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &specs); err != nil {
			return fmt.Errorf("agents.json: agents: %w", err)
		}
	}
	replaced := false
	for i := range specs {
		if specs[i].Name == s.Name {
			specs[i] = s
			replaced = true
			break
		}
	}
	if !replaced {
		specs = append(specs, s)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	doc["agents"] = specs
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// Delete removes one definition by name (no error when absent).
func Delete(path, name string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	doc := map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("agents.json: %w", err)
	}
	raw, _ := json.Marshal(doc["agents"])
	var specs []Spec
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &specs); err != nil {
			return fmt.Errorf("agents.json: agents: %w", err)
		}
	}
	kept := specs[:0]
	for _, s := range specs {
		if s.Name != name {
			kept = append(kept, s)
		}
	}
	doc["agents"] = kept
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}
