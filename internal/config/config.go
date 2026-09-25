// Package config resolves scode's configuration: the ~/.scode layout,
// settings.json, and API key resolution (auth file -> environment).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"scode/internal/llm"
	"scode/internal/llm/anthropic"
	"scode/internal/llm/openaic"
)

// Dir returns the scode config directory (~/.scode, SCODE_DIR override).
func Dir() (string, error) {
	if p := os.Getenv("SCODE_DIR"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".scode"), nil
}

// EnsureDir creates the config directory tree.
func EnsureDir() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// Settings mirrors settings.json (global). Fields stay minimal until the
// CLI grows knobs; unknown keys round-trip untouched.
type Settings struct {
	DefaultProvider      string                    `json:"defaultProvider,omitempty"` // anthropic | openai-compat
	DefaultModel         string                    `json:"defaultModel,omitempty"`
	DefaultThinkingLevel string                    `json:"defaultThinkingLevel,omitempty"` // off | low | medium | high
	CompactionTokens     int                       `json:"compactionTokens,omitempty"`     // 0 = default threshold, negative = disabled
	Providers            map[string]ProviderConfig `json:"providers,omitempty"`
}

// ProviderConfig carries per-provider endpoint and model defaults.
type ProviderConfig struct {
	BaseURL       string       `json:"baseUrl,omitempty"`
	APIKey        string       `json:"apiKey,omitempty"`
	Model         string       `json:"model,omitempty"`
	ContextWindow int          `json:"contextWindow,omitempty"` // tokens; drives the compaction threshold
	Pricing       *llm.Pricing `json:"pricing,omitempty"`       // USD per million tokens
}

// LoadSettings reads settings.json, returning defaults when absent.
func LoadSettings() (*Settings, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return &Settings{}, nil
		}
		return nil, err
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("settings.json: %w", err)
	}
	return &s, nil
}

// Save writes settings.json (pretty-printed).
func (s *Settings) Save() error {
	dir, err := EnsureDir()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "settings.json"), append(b, '\n'), 0o644)
}

// envKeys per provider, checked after settings (pi's auth resolution
// order: auth file/config first, env as fallback).
var envKeys = map[string][]string{
	"anthropic":     {"SCODE_ANTHROPIC_KEY", "ANTHROPIC_API_KEY"},
	"openai-compat": {"SCODE_OPENAI_KEY", "OPENAI_COMPAT_API_KEY", "OPENAI_API_KEY"},
}

// ResolveProvider builds the concrete provider from settings + env.
// provider may be empty (defaults apply), model likewise.
func ResolveProvider(s *Settings, providerName, modelName string) (llm.Provider, string, string, error) {
	if providerName == "" {
		providerName = s.DefaultProvider
	}
	switch providerName {
	case "", "anthropic":
		providerName = "anthropic"
		key, base := "", ""
		if pc := s.Providers["anthropic"]; pc != (ProviderConfig{}) {
			key, base = pc.APIKey, pc.BaseURL
			if modelName == "" {
				modelName = pc.Model
			}
		}
		if key == "" {
			for _, k := range envKeys["anthropic"] {
				if v := os.Getenv(k); v != "" {
					key = v
					break
				}
			}
		}
		if base == "" {
			base = os.Getenv("SCODE_ANTHROPIC_BASE_URL")
		}
		if modelName == "" {
			modelName = "claude-sonnet-4-5"
		}
		return anthropic.New(key, base), modelName, providerName, nil

	case "openai-compat", "openai":
		providerName = "openai-compat"
		key, base := "", ""
		if pc := s.Providers["openai-compat"]; pc != (ProviderConfig{}) {
			key, base = pc.APIKey, pc.BaseURL
			if modelName == "" {
				modelName = pc.Model
			}
		}
		if key == "" {
			for _, k := range envKeys["openai-compat"] {
				if v := os.Getenv(k); v != "" {
					key = v
					break
				}
			}
		}
		if base == "" {
			base = os.Getenv("SCODE_OPENAI_BASE_URL")
		}
		if modelName == "" {
			modelName = "gpt-5.2"
		}
		return openaic.New(key, base), modelName, providerName, nil

	default:
		return nil, "", "", fmt.Errorf("unknown provider %q (want anthropic or openai-compat)", providerName)
	}
}

// ShellEnv exposes session facts to tools (cache rule 4: volatile data
// travels through env, never the prompt).
func ShellEnv(sessionID, provider, model string) map[string]string {
	return map[string]string{
		"SCODE_SESSION_ID": sessionID,
		"SCODE_PROVIDER":   provider,
		"SCODE_MODEL":      model,
		"SCODE_OS":         runtime.GOOS,
	}
}
