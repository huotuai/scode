package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The embedded default catalog parses and validates (unique vendors,
// the three protocols, legal effort levels), and carries the preset
// parameters the /models flow writes into settings.
func TestPresetCatalogEmbedded(t *testing.T) {
	t.Setenv("SCODE_DIR", t.TempDir()) // no override file
	cat, err := LoadPresetCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Vendors) < 2 {
		t.Fatalf("vendors = %d, want several", len(cat.Vendors))
	}
	protocols := map[string]bool{}
	for _, v := range cat.Vendors {
		protocols[v.Protocol] = true
		if v.Name == "" {
			t.Errorf("vendor %s: name missing", v.ID)
		}
		for _, pm := range v.Models {
			if pm.ContextWindow <= 0 || pm.MaxTokens <= 0 {
				t.Errorf("%s:%s context %d / output %d not preset", v.ID, pm.ID, pm.ContextWindow, pm.MaxTokens)
			}
			if pm.Reasoning && len(pm.Efforts) == 0 {
				t.Errorf("%s:%s reasoning model without efforts", v.ID, pm.ID)
			}
		}
	}
	// All three wire modes are represented in the default catalog.
	for _, p := range []string{"anthropic", "openai-responses", "openai-compat"} {
		if !protocols[p] {
			t.Errorf("protocol %s missing from the default catalog", p)
		}
	}
	// Effort labels vary per model (the per-model marks).
	ds := cat.FindVendor("deepseek")
	if ds == nil {
		t.Fatal("deepseek vendor missing")
	}
	found := false
	for _, pm := range ds.Models {
		if pm.ID == "deepseek-reasoner" {
			found = true
			if pm.EffortLabel("low") != "思考" {
				t.Errorf("deepseek-reasoner low label = %q, want 思考", pm.EffortLabel("low"))
			}
		}
	}
	if !found {
		t.Error("deepseek-reasoner missing")
	}
}

// A user override at ~/.scode/models.json replaces the embedded
// catalog.
func TestPresetCatalogOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SCODE_DIR", dir)
	over := `{"vendors":[{"id":"mine","name":"My Vendor","protocol":"openai-compat","baseUrl":"http://x","models":[{"id":"m1","contextWindow":1000,"maxTokens":100}]}]}`
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(over), 0o644); err != nil {
		t.Fatal(err)
	}
	cat, err := LoadPresetCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Vendors) != 1 || cat.Vendors[0].ID != "mine" {
		t.Fatalf("override not in effect: %+v", cat.Vendors)
	}
}

// An invalid override errors loudly (no silent fallback): unknown
// protocol, duplicate vendor id, illegal effort level.
func TestPresetCatalogInvalid(t *testing.T) {
	cases := map[string]string{
		"protocol":   `{"vendors":[{"id":"a","protocol":"grpc","models":[{"id":"m"}]}]}`,
		"duplicate":  `{"vendors":[{"id":"a","protocol":"anthropic","models":[{"id":"m"}]},{"id":"a","protocol":"anthropic","models":[{"id":"m"}]}]}`,
		"effort":     `{"vendors":[{"id":"a","protocol":"anthropic","models":[{"id":"m","efforts":[{"level":"extreme","label":"极"}]}]}]}`,
		"no models":  `{"vendors":[{"id":"a","protocol":"anthropic","models":[]}]}`,
		"empty id":   `{"vendors":[{"id":"a","protocol":"anthropic","models":[{"id":""}]}]}`,
		"def effort": `{"vendors":[{"id":"a","protocol":"anthropic","models":[{"id":"m","defaultEffort":"x"}]}]}`,
	}
	for name, doc := range cases {
		dir := t.TempDir()
		t.Setenv("SCODE_DIR", dir)
		if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadPresetCatalog(); err == nil {
			t.Errorf("%s: invalid catalog accepted", name)
		} else if !strings.Contains(err.Error(), "models.json") {
			t.Errorf("%s: error does not name the override file: %v", name, err)
		}
	}
}

// ProfileName builds the settings providers key for a preset pair.
func TestProfileName(t *testing.T) {
	if got := ProfileName("anthropic", "claude-sonnet-4-5"); got != "anthropic:claude-sonnet-4-5" {
		t.Fatalf("ProfileName = %q", got)
	}
}
