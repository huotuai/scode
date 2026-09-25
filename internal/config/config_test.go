package config

import (
	"os"
	"testing"
)

// Model resolution priority: flag > providers.<name>.model > defaultModel
// > protocol constant.
func TestResolveProviderModelPriority(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "k")
	t.Setenv("OPENAI_COMPAT_API_KEY", "k")

	s := &Settings{
		DefaultModel: "global-default",
		Providers: map[string]ProviderConfig{
			"openai-compat": {Model: "provider-model"},
		},
	}

	_, model, _, err := ResolveProvider(s, "anthropic", "flag-model")
	if err != nil || model != "flag-model" {
		t.Fatalf("flag must win: %q %v", model, err)
	}
	_, model, _, _ = ResolveProvider(s, "openai-compat", "")
	if model != "provider-model" {
		t.Fatalf("provider model must beat defaultModel: %q", model)
	}
	_, model, _, _ = ResolveProvider(s, "anthropic", "")
	if model != "global-default" {
		t.Fatalf("defaultModel must be honored: %q", model)
	}
	_, model, _, _ = ResolveProvider(&Settings{}, "anthropic", "")
	if model != "claude-sonnet-4-5" {
		t.Fatalf("constant fallback wrong: %q", model)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SCODE_DIR", dir)
	s := &Settings{
		DefaultProvider:      "openai-compat",
		DefaultModel:         "glm-4.6",
		DefaultThinkingLevel: "high",
		CompactionTokens:     60_000,
		Providers: map[string]ProviderConfig{
			"openai-compat": {
				BaseURL:       "https://open.bigmodel.cn/api/paas/v4",
				APIKey:        "sk-x",
				Model:         "glm-4.6",
				ContextWindow: 200_000,
			},
		},
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultModel != "glm-4.6" || got.DefaultThinkingLevel != "high" || got.CompactionTokens != 60_000 {
		t.Fatalf("round trip lost fields: %+v", got)
	}
	pc := got.Providers["openai-compat"]
	if pc.ContextWindow != 200_000 || pc.Model != "glm-4.6" || pc.BaseURL == "" {
		t.Fatalf("provider config lost: %+v", pc)
	}
	if _, err := os.Stat(dir + "/settings.json"); err != nil {
		t.Fatal("settings.json not written")
	}
}
