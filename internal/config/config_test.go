package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	// Absent file → empty settings, no error.
	if s, err := LoadProjectSettings(dir); err != nil || s.Permissions != nil {
		t.Fatalf("absent: %+v %v", s, err)
	}
	if err := AddProjectAllowRule(dir, "bash(git status)"); err != nil {
		t.Fatal(err)
	}
	// Idempotent: no duplicate entries.
	if err := AddProjectAllowRule(dir, "bash(git status)"); err != nil {
		t.Fatal(err)
	}
	if err := AddProjectAllowRule(dir, "write(src/**)"); err != nil {
		t.Fatal(err)
	}
	s, err := LoadProjectSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bash(git status)", "write(src/**)"}
	got := s.Permissions.Allow
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("allow=%v want %v", got, want)
	}
}

func TestAddProjectAllowRulePreservesUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".scode"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := ProjectSettingsPath(dir)
	if err := os.WriteFile(path, []byte(`{"note":"keep me","permissions":{"deny":["bash(rm:*)"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AddProjectAllowRule(dir, "bash(ls)"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	s, err := LoadProjectSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Permissions.Deny) != 1 || s.Permissions.Deny[0] != "bash(rm:*)" {
		t.Fatalf("existing deny lost: %v", s.Permissions.Deny)
	}
	if len(s.Permissions.Allow) != 1 {
		t.Fatalf("allow missing: %v", s.Permissions.Allow)
	}
	if !strings.Contains(string(data), "keep me") {
		t.Fatal("unknown key dropped:\n" + string(data))
	}
}

func TestSetProjectSandboxMode(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".scode"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := ProjectSettingsPath(dir)
	if err := os.WriteFile(path, []byte(`{"note":"keep me","permissions":{"deny":["bash(rm:*)"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetProjectSandboxMode(dir, "read-only"); err != nil {
		t.Fatal(err)
	}
	s, err := LoadProjectSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Sandbox == nil || s.Sandbox.Mode != "read-only" {
		t.Fatalf("sandbox mode not persisted: %+v", s.Sandbox)
	}
	// Unknown keys and sibling sections survive the rewrite.
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "keep me") {
		t.Fatal("unknown key dropped:\n" + string(data))
	}
	if len(s.Permissions.Deny) != 1 {
		t.Fatalf("permissions section lost: %v", s.Permissions)
	}
	// A second switch overwrites the mode in place.
	if err := SetProjectSandboxMode(dir, "danger-full-access"); err != nil {
		t.Fatal(err)
	}
	s, err = LoadProjectSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Sandbox.Mode != "danger-full-access" {
		t.Fatalf("mode not updated: %q", s.Sandbox.Mode)
	}
}

func TestSetProjectSandboxModeCreatesTree(t *testing.T) {
	dir := t.TempDir() // no .scode yet
	if err := SetProjectSandboxMode(dir, "workspace-write"); err != nil {
		t.Fatal(err)
	}
	s, err := LoadProjectSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Sandbox == nil || s.Sandbox.Mode != "workspace-write" {
		t.Fatalf("sandbox mode not persisted: %+v", s.Sandbox)
	}
}

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

// settings.retry.provider maps onto the transport config (pi's
// settings.retry.provider): timeout, retries→attempts, delay cap.
func TestResolveProviderNewAdapters(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "gk")
	t.Setenv("OPENAI_API_KEY", "ok")
	t.Setenv("AZURE_OPENAI_API_KEY", "ak")

	p, model, name, err := ResolveProvider(&Settings{}, "google", "")
	if err != nil || model != "gemini-3.1-pro-preview" || name != "google" {
		t.Fatalf("google: %q %q %v", model, name, err)
	}
	if p.Name() != "google" {
		t.Fatalf("provider name = %q", p.Name())
	}

	p, model, name, err = ResolveProvider(&Settings{}, "openai-responses", "")
	if err != nil || model != "gpt-5.5" || name != "openai-responses" {
		t.Fatalf("openai-responses: %q %q %v", model, name, err)
	}
	if p.Name() != "openai-responses" {
		t.Fatalf("provider name = %q", p.Name())
	}

	// "azure" is accepted as an alias of azure-openai-responses.
	p, model, name, err = ResolveProvider(&Settings{}, "azure", "")
	if err != nil || model != "gpt-5.4" || name != "azure-openai-responses" {
		t.Fatalf("azure: %q %q %v", model, name, err)
	}
	if p.Name() != "azure-openai-responses" {
		t.Fatalf("provider name = %q", p.Name())
	}
}

func TestResolveTransport(t *testing.T) {
	d := ResolveTransport(&Settings{})
	if d.TimeoutMs != 600_000 || d.MaxAttempts != 4 || d.MaxRetryDelayMs != 60_000 {
		t.Fatalf("defaults = %+v", d)
	}
	s := &Settings{Retry: &RetrySettings{Provider: &ProviderRetrySettings{
		TimeoutMs: 30_000, MaxRetries: 2, MaxRetryDelayMs: 5_000,
	}}}
	got := ResolveTransport(s)
	if got.TimeoutMs != 30_000 || got.MaxAttempts != 3 || got.MaxRetryDelayMs != 5_000 {
		t.Fatalf("resolved = %+v", got)
	}
}

// A custom provider key resolves as an openai-compatible endpoint by
// default (its Protocol field selects another adapter).
func TestResolveProviderCustomAlias(t *testing.T) {
	s := &Settings{
		Providers: map[string]ProviderConfig{
			"kimi": {APIKey: "sk-k", BaseURL: "https://api.kimi.com/coding/v1", Model: "k3", ContextWindow: 1_000_000},
		},
	}
	p, model, name, err := ResolveProvider(s, "kimi", "")
	if err != nil {
		t.Fatalf("custom alias: %v", err)
	}
	if name != "kimi" || model != "k3" {
		t.Fatalf("resolved = %q/%q, want kimi/k3", name, model)
	}
	if p.Name() != "openai-compat" {
		t.Fatalf("custom default protocol = %q, want openai-compat", p.Name())
	}

	// Explicit model overrides the provider default.
	_, model, _, err = ResolveProvider(s, "kimi", "k3-turbo")
	if err != nil || model != "k3-turbo" {
		t.Fatalf("explicit model = %q %v", model, err)
	}

	// Unknown name with no settings entry is an error.
	if _, _, _, err := ResolveProvider(s, "nope", ""); err == nil {
		t.Fatal("unknown provider should fail")
	}
}

// ListModels surfaces every configured provider plus the default,
// deduplicated and stable.
func TestListModels(t *testing.T) {
	s := &Settings{
		DefaultProvider: "openai-compat",
		DefaultModel:    "deepseek-flash",
		Providers: map[string]ProviderConfig{
			"kimi":          {Model: "k3"},
			"openai-compat": {Model: "deepseek-flash"},
		},
	}
	got := ListModels(s)
	if len(got) != 2 {
		t.Fatalf("models = %+v, want 2 unique", got)
	}
	// Sorted by label: "kimi / k3" before "openai-compat / deepseek-flash".
	if got[0].Provider != "kimi" || got[0].Model != "k3" {
		t.Fatalf("first = %+v", got[0])
	}
	if got[1].Provider != "openai-compat" || got[1].Model != "deepseek-flash" {
		t.Fatalf("second = %+v", got[1])
	}

	// No providers configured: protocol defaults appear instead.
	empty := ListModels(&Settings{})
	if len(empty) < 5 {
		t.Fatalf("empty settings = %+v, want built-in defaults", empty)
	}
}

// Provider CRUD round-trips through settings.json, preserves unknown
// keys, supports rename, and validates protocol/name.
func TestProviderCRUD(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SCODE_DIR", dir)
	seed := `{"note":"keep","defaultProvider":"openai-compat","providers":{"openai-compat":{"apiKey":"x","model":"m"}}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create.
	if err := UpsertProvider("kimi", ProviderConfig{APIKey: "sk-k", BaseURL: "https://api.kimi.com/coding/v1", Model: "k3"}, ""); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if s.Providers["kimi"].Model != "k3" {
		t.Fatalf("kimi not persisted: %+v", s.Providers)
	}

	// Unknown top-level keys survive the map-level write.
	data, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	if !strings.Contains(string(data), "keep") {
		t.Fatalf("unknown key dropped:\n%s", data)
	}

	// Rename: the old key is removed in the same write.
	if err := UpsertProvider("kimi-k3", ProviderConfig{APIKey: "sk-k", Model: "k3"}, "kimi"); err != nil {
		t.Fatal(err)
	}
	s, _ = LoadSettings()
	if _, ok := s.Providers["kimi"]; ok {
		t.Fatal("rename left the old key")
	}
	if s.Providers["kimi-k3"].Model != "k3" {
		t.Fatalf("renamed profile = %+v", s.Providers)
	}
	if got := ListProfiles(s); len(got) != 2 || got[0].Name != "kimi-k3" || got[1].Name != "openai-compat" {
		t.Fatalf("ListProfiles = %+v", got)
	}

	// Validation.
	if err := UpsertProvider("bad", ProviderConfig{Protocol: "nope"}, ""); err == nil {
		t.Fatal("unknown protocol should be rejected")
	}
	if err := UpsertProvider("  ", ProviderConfig{}, ""); err == nil {
		t.Fatal("empty name should be rejected")
	}

	// Delete leaves unrelated profiles intact.
	if err := DeleteProvider("kimi-k3"); err != nil {
		t.Fatal(err)
	}
	s, _ = LoadSettings()
	if _, ok := s.Providers["kimi-k3"]; ok {
		t.Fatal("delete left the key")
	}
	if _, ok := s.Providers["openai-compat"]; !ok {
		t.Fatal("delete removed an unrelated key")
	}
}

// SetDefault writes defaultProvider/defaultModel without dropping unknown
// settings keys.
func TestSetDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SCODE_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"note":"keep","defaultProvider":"openai-compat"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetDefault("kimi", "k3"); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if s.DefaultProvider != "kimi" || s.DefaultModel != "k3" {
		t.Fatalf("default = %q/%q", s.DefaultProvider, s.DefaultModel)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	if !strings.Contains(string(data), "keep") {
		t.Fatalf("unknown key dropped:\n%s", data)
	}
	if err := SetDefault("", "x"); err == nil {
		t.Fatal("empty provider should be rejected")
	}
}

// SetWebSearchProvider round-trips through the web section, preserves
// unknown keys (top-level and in-section), validates values, and ""
// deletes the key.
func TestSetWebSearchProvider(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SCODE_DIR", dir)
	seed := `{"note":"keep","web":{"allowPrivateNetwork":true}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SetWebSearchProvider("brave"); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if s.Web == nil || s.Web.SearchProvider != "brave" {
		t.Fatalf("provider not persisted: %+v", s.Web)
	}
	if !s.Web.AllowPrivate() {
		t.Fatal("existing web section keys dropped")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	if !strings.Contains(string(data), `"note": "keep"`) {
		t.Fatal("unknown top-level key dropped")
	}

	if err := SetWebSearchProvider("bing"); err == nil {
		t.Fatal("unknown provider accepted")
	}
	if err := SetWebSearchProvider(""); err != nil {
		t.Fatal(err)
	}
	s, _ = LoadSettings()
	if s.Web != nil && s.Web.SearchProvider != "" {
		t.Fatalf("reset left %q", s.Web.SearchProvider)
	}
}
