// Preset model catalog: a fixed JSON file groups preset models by
// vendor. The /models command walks it (vendor → API key → model →
// confirm) and lands the pick in settings.json providers, so /model
// sees the configured profile afterwards.
//
// Resolution order: ~/.scode/models.json (user override) when present,
// else the embedded default catalog. Either way the document is
// validated: unique vendor ids, one of the three wire protocols
// (anthropic / openai-responses / openai-compat), effort levels inside
// the session vocabulary (off|low|medium|high).
package config

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed models.json
var defaultPresetJSON []byte

// PresetEffort is one selectable reasoning strength of a preset model:
// Level is the session vocabulary value, Label the per-model display
// mark (different models mark the same strength differently — 关闭/无,
// 低/思考, …).
type PresetEffort struct {
	Level string `json:"level"`
	Label string `json:"label"`
}

// PresetModel is one preset model entry: the wire id plus the profile
// parameters that land in settings (context, output cap, reasoning).
type PresetModel struct {
	ID            string         `json:"id"`
	Name          string         `json:"name,omitempty"`
	ContextWindow int            `json:"contextWindow,omitempty"`
	MaxTokens     int            `json:"maxTokens,omitempty"`
	Reasoning     bool           `json:"reasoning,omitempty"`
	ImageInput    *bool          `json:"imageInput,omitempty"`
	Efforts       []PresetEffort `json:"efforts,omitempty"`       // selectable strengths (reasoning models)
	DefaultEffort string         `json:"defaultEffort,omitempty"` // preselected effort level
}

// PresetVendor is one vendor group: the wire protocol and endpoint all
// its models share, plus the key-input hint.
type PresetVendor struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Protocol string        `json:"protocol"` // anthropic | openai-responses | openai-compat
	BaseURL  string        `json:"baseUrl,omitempty"`
	KeyHint  string        `json:"keyHint,omitempty"` // placeholder shown at the key stage
	Models   []PresetModel `json:"models"`
}

// PresetCatalog is the whole preset document.
type PresetCatalog struct {
	Vendors []PresetVendor `json:"vendors"`
}

// presetProtocols are the three wire modes the catalog may declare.
var presetProtocols = map[string]bool{
	"anthropic": true, "openai-responses": true, "openai-compat": true,
}

// ProfileName is the settings providers key for a configured preset
// model: "vendor:model" — one vendor carries many models, each an
// independently configured profile.
func ProfileName(vendorID, modelID string) string {
	return vendorID + ":" + modelID
}

// presetEffortLevels are the legal effort levels (the session's
// thinking vocabulary).
var presetEffortLevels = map[string]bool{"off": true, "low": true, "medium": true, "high": true}

// LoadPresetCatalog reads the preset catalog: the user override at
// ~/.scode/models.json when it exists, else the embedded default. The
// document is validated either way (an invalid override errors loudly
// rather than silently falling back).
func LoadPresetCatalog() (*PresetCatalog, error) {
	data := defaultPresetJSON
	source := "embedded preset catalog"
	if dir, err := Dir(); err == nil {
		if over, oerr := os.ReadFile(filepath.Join(dir, "models.json")); oerr == nil && len(over) > 0 {
			data, source = over, filepath.Join(dir, "models.json")
		}
	}
	var c PresetCatalog
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	return &c, nil
}

// Validate checks the catalog invariants (unique vendors, known
// protocols, legal effort levels, non-empty model ids).
func (c *PresetCatalog) Validate() error {
	if len(c.Vendors) == 0 {
		return fmt.Errorf("no vendors")
	}
	seen := map[string]bool{}
	for i, v := range c.Vendors {
		where := fmt.Sprintf("vendor %d (%q)", i, v.ID)
		if strings.TrimSpace(v.ID) == "" {
			return fmt.Errorf("vendor %d: id is required", i)
		}
		if seen[v.ID] {
			return fmt.Errorf("vendor %q: duplicate id", v.ID)
		}
		seen[v.ID] = true
		if !presetProtocols[v.Protocol] {
			return fmt.Errorf("%s: unknown protocol %q (want anthropic|openai-responses|openai-compat)", where, v.Protocol)
		}
		if len(v.Models) == 0 {
			return fmt.Errorf("%s: no models", where)
		}
		for j, pm := range v.Models {
			mwhere := fmt.Sprintf("%s model %d (%q)", where, j, pm.ID)
			if strings.TrimSpace(pm.ID) == "" {
				return fmt.Errorf("%s model %d: id is required", where, j)
			}
			for _, e := range pm.Efforts {
				if !presetEffortLevels[e.Level] {
					return fmt.Errorf("%s: unknown effort level %q (want off|low|medium|high)", mwhere, e.Level)
				}
			}
			if pm.DefaultEffort != "" && !presetEffortLevels[pm.DefaultEffort] {
				return fmt.Errorf("%s: unknown defaultEffort %q", mwhere, pm.DefaultEffort)
			}
		}
	}
	return nil
}

// FindVendor returns the vendor with the given id (nil when absent).
func (c *PresetCatalog) FindVendor(id string) *PresetVendor {
	for i := range c.Vendors {
		if c.Vendors[i].ID == id {
			return &c.Vendors[i]
		}
	}
	return nil
}

// EffortLabel maps an effort level to this model's display mark,
// falling back to the level itself.
func (pm *PresetModel) EffortLabel(level string) string {
	for _, e := range pm.Efforts {
		if e.Level == level {
			return e.Label
		}
	}
	return level
}

// DisplayName prefers the human name, falling back to the wire id.
func (pm *PresetModel) DisplayName() string {
	if pm.Name != "" {
		return pm.Name
	}
	return pm.ID
}
