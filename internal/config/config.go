// Package config resolves scode's configuration: the ~/.scode layout,
// settings.json, and API key resolution (auth file -> environment).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"scode/internal/agent"
	"scode/internal/llm"
	"scode/internal/llm/anthropic"
	"scode/internal/llm/google"
	"scode/internal/llm/openaic"
	"scode/internal/llm/responses"
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
	DefaultProvider      string                    `json:"defaultProvider,omitempty"` // providers.<name> key (built-in protocol or custom alias); empty = anthropic
	DefaultModel         string                    `json:"defaultModel,omitempty"`
	DefaultThinkingLevel string                    `json:"defaultThinkingLevel,omitempty"` // off | low | medium | high
	CompactionTokens     int                       `json:"compactionTokens,omitempty"`     // 0 = default threshold, negative = disabled
	KeepRecentTokens     int                       `json:"keepRecentTokens,omitempty"`     // verbatim tail kept across compaction; 0 = default 20000, negative = keep nothing
	Skills               []string                  `json:"skills,omitempty"`               // extra skill files/directories (pi's settings.skills)
	Permissions          *Permissions              `json:"permissions,omitempty"`          // tool-call rules (design: docs/design-permission-plan-desktop.md)
	Sandbox              *SandboxConfig            `json:"sandbox,omitempty"`              // file-effect sandbox default (dsh sandbox-policy port)
	Retry                *RetrySettings            `json:"retry,omitempty"`                // pi's settings.retry
	ClipboardWatch       *bool                     `json:"clipboardWatch,omitempty"`       // TUI: auto-attach clipboard images (terminals that swallow ctrl+v); default true
	UpdateRepo           string                    `json:"updateRepo,omitempty"`           // GitHub "org/repo" release source for scode update (env SCODE_UPDATE_REPO overrides)
	UpdateCheck          *bool                     `json:"updateCheck,omitempty"`          // background new-version notice; default on
	Providers            map[string]ProviderConfig `json:"providers,omitempty"`

	// /config panel (the basic-configuration surface). Pointer booleans:
	// nil = the documented default.
	AutoCompact       *bool `json:"autoCompact,omitempty"`          // context auto-compaction; default on
	LogRetentionDays  int   `json:"logRetentionDays,omitempty"`     // session-log cleanup period; 0 = never prune
	AutoMemory        *bool `json:"autoMemory,omitempty"`           // AI remembers key facts across sessions; default on
	TypedMemory       *bool `json:"typedMemory,omitempty"`          // memories stored by category; default on
	MemoryRelevance   *bool `json:"memoryRelevance,omitempty"`      // manual relevance selection; default off (AI picks)
	MemoryAutoExtract *bool `json:"memoryAutoExtraction,omitempty"` // auto-extract memories at session end; default off
	RewindCheckpoints *bool `json:"rewindCheckpoints,omitempty"`    // rewind code to AI checkpoints; default on
}

// The /config booleans with their defaults (nil = default).
func (s *Settings) AutoCompactOn() bool     { return s.AutoCompact == nil || *s.AutoCompact }
func (s *Settings) AutoMemoryOn() bool      { return s.AutoMemory == nil || *s.AutoMemory }
func (s *Settings) TypedMemoryOn() bool     { return s.TypedMemory == nil || *s.TypedMemory }
func (s *Settings) MemoryRelevanceOn() bool { return s.MemoryRelevance != nil && *s.MemoryRelevance }
func (s *Settings) MemoryAutoExtractOn() bool {
	return s.MemoryAutoExtract != nil && *s.MemoryAutoExtract
}
func (s *Settings) RewindCheckpointsOn() bool {
	return s.RewindCheckpoints == nil || *s.RewindCheckpoints
}

// UpdateCheckOn reports whether the background new-version notice runs.
func (s *Settings) UpdateCheckOn() bool { return s.UpdateCheck == nil || *s.UpdateCheck }

// ResolveUpdateRepo picks the GitHub release source: SCODE_UPDATE_REPO
// env > settings updateRepo > the baked-in default.
func (s *Settings) ResolveUpdateRepo(defaultRepo string) string {
	if v := os.Getenv("SCODE_UPDATE_REPO"); v != "" {
		return v
	}
	if s.UpdateRepo != "" {
		return s.UpdateRepo
	}
	return defaultRepo
}

// SandboxConfig mirrors the sandbox section: the deployment default
// file-effect mode. Empty/absent means workspace-write (the shipped
// posture: file mutations fenced to the workspace plus permitted temp
// areas, wider retries need approval); the confined modes need a runner
// backend for bash (fail-closed without one).
type SandboxConfig struct {
	Mode string `json:"mode,omitempty"` // read-only | workspace-write | danger-full-access
}

// Permissions mirrors the permissions section: Claude Code-style
// tool(pattern) rules evaluated deny > ask > allow, with defaultMode as
// the unmatched fallback (default | bypass).
type Permissions struct {
	DefaultMode string   `json:"defaultMode,omitempty"`
	Allow       []string `json:"allow,omitempty"`
	Ask         []string `json:"ask,omitempty"`
	Deny        []string `json:"deny,omitempty"`
}

// ProjectSettingsPath is the project-level settings file (.scode/settings.json
// under the working directory). Only the permissions section is read from
// it today; lists append onto the user-level settings (project rules are
// listed later, so user rules win on equal footing within a list).
func ProjectSettingsPath(cwd string) string {
	return filepath.Join(cwd, ".scode", "settings.json")
}

// LoadProjectSettings reads cwd/.scode/settings.json, returning empty
// settings when absent.
func LoadProjectSettings(cwd string) (*Settings, error) {
	data, err := os.ReadFile(ProjectSettingsPath(cwd))
	if err != nil {
		if os.IsNotExist(err) {
			return &Settings{}, nil
		}
		return nil, err
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf(".scode/settings.json: %w", err)
	}
	return &s, nil
}

// AddProjectAllowRule appends one allow rule to the project settings,
// preserving unknown keys (map-level read-modify-write; the 'p'
// approval key writes here).
func AddProjectAllowRule(cwd, rule string) error {
	path := ProjectSettingsPath(cwd)
	doc := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf(".scode/settings.json: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	perms, _ := doc["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
		doc["permissions"] = perms
	}
	allow, _ := perms["allow"].([]any)
	for _, existing := range allow {
		if existing == rule {
			return nil // already persisted
		}
	}
	perms["allow"] = append(allow, rule)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// RetrySettings mirrors pi's settings.retry.
type RetrySettings struct {
	Enabled         *bool                  `json:"enabled,omitempty"`         // default true
	MaxRetries      int                    `json:"maxRetries,omitempty"`      // default 3
	BaseDelayMs     int                    `json:"baseDelayMs,omitempty"`     // default 2000 (exponential: 2s, 4s, 8s)
	MaxAgentDelayMs int                    `json:"maxAgentDelayMs,omitempty"` // default 60000
	Provider        *ProviderRetrySettings `json:"provider,omitempty"`        // pi's settings.retry.provider (transport level)
}

// ProviderRetrySettings mirrors pi's settings.retry.provider:
// transport-level request timeout, retry attempts, and the backoff cap.
type ProviderRetrySettings struct {
	TimeoutMs       int `json:"timeoutMs,omitempty"`       // per-request timeout; 0 = default
	MaxRetries      int `json:"maxRetries,omitempty"`      // transport retry attempts; 0 = default
	MaxRetryDelayMs int `json:"maxRetryDelayMs,omitempty"` // backoff cap; default 60000
}

// ProviderConfig carries per-provider endpoint and model defaults.
type ProviderConfig struct {
	BaseURL        string       `json:"baseUrl,omitempty"`
	APIKey         string       `json:"apiKey,omitempty"`
	Model          string       `json:"model,omitempty"`
	Protocol       string       `json:"protocol,omitempty"`       // protocol for a custom provider key (default openai-compat)
	ContextWindow  int          `json:"contextWindow,omitempty"`  // tokens; drives the compaction threshold
	MaxTokens      int          `json:"maxTokens,omitempty"`      // model output cap (pi's catalog maxTokens): default max_tokens and the thinking-budget clamp
	Reasoning      bool         `json:"reasoning,omitempty"`      // model is thinking-capable (pi's model.reasoning); gates reasoning params on the responses/google adapters
	ImageInput     *bool        `json:"imageInput,omitempty"`     // model accepts images (pi's model.input); default anthropic=true, openai-compat=false
	MidConvoSystem bool         `json:"midConvoSystem,omitempty"` // endpoint verified to accept mid-conversation system messages (pi's compat.supportsMidConvoSystemMessages): system deltas ride in place, keeping the request head cache-stable
	Pricing        *llm.Pricing `json:"pricing,omitempty"`        // USD per million tokens
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

// ---------------------------------------------------------------------------
// provider (model profile) CRUD
//
// These edits go through a map-level read-modify-write so unknown keys in
// settings.json survive a round trip (a plain struct save would drop
// them). Each providers.<name> entry is one selectable model profile.
// ---------------------------------------------------------------------------

// readSettingsDoc loads settings.json as a generic map (empty when
// absent).
func readSettingsDoc() (map[string]any, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	doc := map[string]any{}
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return doc, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return doc, nil
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("settings.json: %w", err)
	}
	return doc, nil
}

// writeSettingsDoc persists the map-level document (pretty-printed).
func writeSettingsDoc(doc map[string]any) error {
	dir, err := EnsureDir()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "settings.json"), append(b, '\n'), 0o644)
}

// knownProtocols are the wire adapters a profile may declare.
var knownProtocols = map[string]bool{
	"anthropic": true, "openai-compat": true, "openai-responses": true,
	"azure-openai-responses": true, "google": true,
}

// UpsertProvider creates or replaces a named model profile. original,
// when set and different from name, is removed in the same write
// (rename support).
func UpsertProvider(name string, pc ProviderConfig, original string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("provider name is required")
	}
	if pc.Protocol != "" && !knownProtocols[pc.Protocol] {
		return fmt.Errorf("unknown protocol %q", pc.Protocol)
	}
	doc, err := readSettingsDoc()
	if err != nil {
		return err
	}
	providers, _ := doc["providers"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
		doc["providers"] = providers
	}
	if original != "" && original != name {
		delete(providers, original)
	}
	providers[name] = pc
	return writeSettingsDoc(doc)
}

// DeleteProvider removes a named model profile (no-op when absent).
func DeleteProvider(name string) error {
	doc, err := readSettingsDoc()
	if err != nil {
		return err
	}
	providers, _ := doc["providers"].(map[string]any)
	if providers == nil {
		return nil
	}
	delete(providers, name)
	return writeSettingsDoc(doc)
}

// SetDefault records defaultProvider/defaultModel, the fallback a session
// resolves when created without an explicit provider/model. Map-level
// write keeps unknown keys.
func SetDefault(provider, model string) error {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return fmt.Errorf("default provider is required")
	}
	doc, err := readSettingsDoc()
	if err != nil {
		return err
	}
	doc["defaultProvider"] = provider
	if model != "" {
		doc["defaultModel"] = model
	} else {
		delete(doc, "defaultModel")
	}
	return writeSettingsDoc(doc)
}

// tuiConfigKeys are the /config panel's persistable keys (validated so
// typos cannot smuggle arbitrary keys into settings.json).
var tuiConfigKeys = map[string]bool{
	"autoCompact": true, "logRetentionDays": true,
	"autoMemory": true, "typedMemory": true, "memoryRelevance": true,
	"memoryAutoExtraction": true, "rewindCheckpoints": true,
	"clipboardWatch": true,
}

// SetTUIConfigKey persists one /config panel key (bool or int value;
// nil deletes the key, restoring the default).
func SetTUIConfigKey(key string, value any) error {
	if !tuiConfigKeys[key] {
		return fmt.Errorf("unknown config key %q", key)
	}
	doc, err := readSettingsDoc()
	if err != nil {
		return err
	}
	if value == nil {
		delete(doc, key)
	} else {
		doc[key] = value
	}
	return writeSettingsDoc(doc)
}

// SetDefaultThinking records defaultThinkingLevel, the effort fresh
// sessions start with ("" removes the key: provider default). Map-level
// write keeps unknown keys.
func SetDefaultThinking(level string) error {
	switch level {
	case "", "off", "low", "medium", "high":
	default:
		return fmt.Errorf("unknown thinking level %q (want off|low|medium|high)", level)
	}
	doc, err := readSettingsDoc()
	if err != nil {
		return err
	}
	if level == "" {
		delete(doc, "defaultThinkingLevel")
	} else {
		doc["defaultThinkingLevel"] = level
	}
	return writeSettingsDoc(doc)
}

// ProviderProfile is one model profile with its identifying name (the
// providers.<name> key). Fields flatten into the same JSON shape as
// ProviderConfig for the settings editor.
type ProviderProfile struct {
	Name string `json:"name"`
	ProviderConfig
}

// ListProfiles returns every configured model profile, sorted by name.
func ListProfiles(s *Settings) []ProviderProfile {
	names := make([]string, 0, len(s.Providers))
	for name := range s.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ProviderProfile, 0, len(names))
	for _, name := range names {
		out = append(out, ProviderProfile{Name: name, ProviderConfig: s.Providers[name]})
	}
	return out
}

// envKeys per provider, checked after settings (pi's auth resolution
// order: auth file/config first, env as fallback).
var envKeys = map[string][]string{
	"anthropic":              {"SCODE_ANTHROPIC_KEY", "ANTHROPIC_API_KEY"},
	"openai-compat":          {"SCODE_OPENAI_KEY", "OPENAI_COMPAT_API_KEY", "OPENAI_API_KEY"},
	"openai-responses":       {"SCODE_OPENAI_RESPONSES_KEY", "OPENAI_API_KEY"},
	"azure-openai-responses": {"SCODE_AZURE_KEY", "AZURE_OPENAI_API_KEY"},
	"google":                 {"SCODE_GOOGLE_KEY", "GEMINI_API_KEY"},
}

// builtinProviders are the protocol-native provider keys (plus their
// accepted aliases) that ResolveProvider's switch handles directly.
var builtinProviders = map[string]bool{
	"anthropic": true, "openai-compat": true, "openai": true,
	"openai-responses": true, "azure-openai-responses": true, "azure": true,
	"google": true, "gemini": true,
}

// protocolDefaults maps a protocol to its fallback model id and base-URL
// environment variable (custom provider keys resolve through it).
var protocolDefaults = map[string]struct{ model, baseEnv string }{
	"anthropic":              {"claude-sonnet-4-5", "SCODE_ANTHROPIC_BASE_URL"},
	"openai-compat":          {"gpt-5.2", "SCODE_OPENAI_BASE_URL"},
	"openai-responses":       {"gpt-5.5", "SCODE_OPENAI_RESPONSES_BASE_URL"},
	"azure-openai-responses": {"gpt-5.4", ""},
	"google":                 {"gemini-3.1-pro-preview", "SCODE_GOOGLE_BASE_URL"},
}

// normalizeProtocol maps provider aliases onto their canonical protocol
// key (the protocolDefaults/envKeys key).
func normalizeProtocol(p string) string {
	switch p {
	case "openai":
		return "openai-compat"
	case "azure":
		return "azure-openai-responses"
	case "gemini":
		return "google"
	}
	return p
}

// newProvider builds the concrete provider for a protocol. Unknown
// protocols return nil.
func newProvider(protocol, key, base string, tr llm.TransportConfig) llm.Provider {
	switch protocol {
	case "anthropic":
		p := anthropic.New(key, base)
		p.Transport = tr
		return p
	case "openai-compat", "openai":
		p := openaic.New(key, base)
		p.Transport = tr
		return p
	case "openai-responses":
		p := responses.NewOpenAI(key, base)
		p.Transport = tr
		return p
	case "azure-openai-responses", "azure":
		p := responses.NewAzure(key, base)
		p.Transport = tr
		return p
	case "google", "gemini":
		p := google.New(key, base)
		p.Transport = tr
		return p
	}
	return nil
}

// ResolveProvider builds the concrete provider from settings + env.
// provider may be empty (defaults apply), model likewise. A provider
// name that is not a built-in protocol is treated as a custom alias for
// the settings.Providers entry of the same name; its Protocol field (or
// openai-compat by default) selects the wire adapter.
func ResolveProvider(s *Settings, providerName, modelName string) (llm.Provider, string, string, error) {
	if providerName == "" {
		providerName = s.DefaultProvider
	}
	if providerName != "" && !builtinProviders[providerName] {
		pc, ok := s.Providers[providerName]
		if !ok {
			return nil, "", "", fmt.Errorf("unknown provider %q (want anthropic, openai-compat, openai-responses, azure-openai-responses, google, or a configured custom key)", providerName)
		}
		protocol := pc.Protocol
		if protocol == "" {
			protocol = "openai-compat"
		}
		protocol = normalizeProtocol(protocol)
		def, ok := protocolDefaults[protocol]
		if !ok {
			return nil, "", "", fmt.Errorf("provider %q: unknown protocol %q", providerName, protocol)
		}
		if modelName == "" {
			modelName = pc.Model
		}
		if modelName == "" {
			modelName = s.DefaultModel
		}
		if modelName == "" {
			modelName = def.model
		}
		key := pc.APIKey
		if key == "" {
			for _, k := range envKeys[protocol] {
				if v := os.Getenv(k); v != "" {
					key = v
					break
				}
			}
		}
		base := pc.BaseURL
		if base == "" && def.baseEnv != "" {
			base = os.Getenv(def.baseEnv)
		}
		p := newProvider(protocol, key, base, ResolveTransport(s))
		if p == nil {
			return nil, "", "", fmt.Errorf("provider %q: unsupported protocol %q", providerName, protocol)
		}
		return p, modelName, providerName, nil
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
			modelName = s.DefaultModel
		}
		if modelName == "" {
			modelName = "claude-sonnet-4-5"
		}
		ap := anthropic.New(key, base)
		ap.Transport = ResolveTransport(s)
		return ap, modelName, providerName, nil

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
			modelName = s.DefaultModel
		}
		if modelName == "" {
			modelName = "gpt-5.2"
		}
		op := openaic.New(key, base)
		op.Transport = ResolveTransport(s)
		return op, modelName, providerName, nil

	case "openai-responses":
		key, base := "", ""
		if pc := s.Providers["openai-responses"]; pc != (ProviderConfig{}) {
			key, base = pc.APIKey, pc.BaseURL
			if modelName == "" {
				modelName = pc.Model
			}
		}
		if key == "" {
			for _, k := range envKeys["openai-responses"] {
				if v := os.Getenv(k); v != "" {
					key = v
					break
				}
			}
		}
		if base == "" {
			base = os.Getenv("SCODE_OPENAI_RESPONSES_BASE_URL")
		}
		if modelName == "" {
			modelName = s.DefaultModel
		}
		if modelName == "" {
			modelName = "gpt-5.5" // pi's defaultModelPerProvider[openai]
		}
		op := responses.NewOpenAI(key, base)
		op.Transport = ResolveTransport(s)
		return op, modelName, providerName, nil

	case "azure-openai-responses", "azure":
		providerName = "azure-openai-responses"
		key, base := "", ""
		if pc := s.Providers["azure-openai-responses"]; pc != (ProviderConfig{}) {
			key, base = pc.APIKey, pc.BaseURL
			if modelName == "" {
				modelName = pc.Model
			}
		}
		if key == "" {
			for _, k := range envKeys["azure-openai-responses"] {
				if v := os.Getenv(k); v != "" {
					key = v
					break
				}
			}
		}
		// Base URL resolution (env chain + normalization) lives in the
		// provider, mirroring pi's resolveAzureConfig.
		if modelName == "" {
			modelName = s.DefaultModel
		}
		if modelName == "" {
			modelName = "gpt-5.4" // pi's defaultModelPerProvider[azure-openai-responses]
		}
		ap := responses.NewAzure(key, base)
		ap.Transport = ResolveTransport(s)
		return ap, modelName, providerName, nil

	case "google", "gemini":
		providerName = "google"
		key, base := "", ""
		if pc := s.Providers["google"]; pc != (ProviderConfig{}) {
			key, base = pc.APIKey, pc.BaseURL
			if modelName == "" {
				modelName = pc.Model
			}
		}
		if key == "" {
			for _, k := range envKeys["google"] {
				if v := os.Getenv(k); v != "" {
					key = v
					break
				}
			}
		}
		if base == "" {
			base = os.Getenv("SCODE_GOOGLE_BASE_URL")
		}
		if modelName == "" {
			modelName = s.DefaultModel
		}
		if modelName == "" {
			modelName = "gemini-3.1-pro-preview" // pi's defaultModelPerProvider[google]
		}
		gp := google.New(key, base)
		gp.Transport = ResolveTransport(s)
		return gp, modelName, providerName, nil

	default:
		return nil, "", "", fmt.Errorf("unknown provider %q (want anthropic, openai-compat, openai-responses, azure-openai-responses, or google)", providerName)
	}
}

// ResolveTransport maps settings.retry.provider onto the transport
// config (pi's defaults where unset).
func ResolveTransport(s *Settings) llm.TransportConfig {
	t := llm.DefaultTransport()
	if s.Retry == nil || s.Retry.Provider == nil {
		return t
	}
	if s.Retry.Provider.TimeoutMs > 0 {
		t.TimeoutMs = s.Retry.Provider.TimeoutMs
	}
	if s.Retry.Provider.MaxRetries > 0 {
		t.MaxAttempts = s.Retry.Provider.MaxRetries + 1 // settings count retries, config counts attempts
	}
	if s.Retry.Provider.MaxRetryDelayMs > 0 {
		t.MaxRetryDelayMs = s.Retry.Provider.MaxRetryDelayMs
	}
	return t
}

// ResolveRetry maps settings.retry onto the agent policy (pi's
// defaults: enabled, 3 retries, 2s base, 60s cap).
func ResolveRetry(s *Settings) agent.RetryPolicy {
	p := agent.DefaultRetryPolicy()
	if s.Retry == nil {
		return p
	}
	if s.Retry.Enabled != nil && !*s.Retry.Enabled {
		p.Enabled = false
	}
	if s.Retry.MaxRetries > 0 {
		p.MaxRetries = s.Retry.MaxRetries
	}
	if s.Retry.BaseDelayMs > 0 {
		p.BaseDelayMs = s.Retry.BaseDelayMs
	}
	if s.Retry.MaxAgentDelayMs > 0 {
		p.MaxAgentDelayMs = s.Retry.MaxAgentDelayMs
	}
	return p
}

// ModelChoice is one selectable provider/model pair (settings.Providers
// entries plus the configured default).
type ModelChoice struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Label    string `json:"label"`
	// Reasoning marks thinking-capable models (pi's model.reasoning):
	// lets clients gate reasoning-effort controls per choice.
	Reasoning bool `json:"reasoning,omitempty"`
}

// ListModels enumerates the switchable models: every configured provider
// with a concrete model id, plus the settings default when it is not
// already covered. Built-in protocols that have no settings entry are
// listed only when no provider is configured at all, so the selector is
// never empty.
func ListModels(s *Settings) []ModelChoice {
	var out []ModelChoice
	seen := map[string]bool{}
	add := func(provider, model string, reasoning bool) {
		if provider == "" || model == "" {
			return
		}
		key := provider + "|" + model
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, ModelChoice{Provider: provider, Model: model, Label: provider + " / " + model, Reasoning: reasoning})
	}

	// Configured providers, in stable order.
	names := make([]string, 0, len(s.Providers))
	for name := range s.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		pc := s.Providers[name]
		model := pc.Model
		if model == "" {
			model = s.DefaultModel
		}
		if model == "" {
			model = defaultModelFor(name, pc.Protocol)
		}
		add(name, model, pc.Reasoning)
	}

	// The configured default (may point at a provider with no entry).
	if s.DefaultProvider != "" {
		model := s.DefaultModel
		reasoning := false
		if pc, ok := s.Providers[s.DefaultProvider]; ok {
			if model == "" {
				model = pc.Model
			}
			reasoning = pc.Reasoning
		}
		if model == "" {
			model = defaultModelFor(s.DefaultProvider, "")
		}
		add(s.DefaultProvider, model, reasoning)
	}

	// Nothing configured: surface the protocol-native defaults so the
	// selector still offers a switch.
	if len(out) == 0 {
		for _, name := range []string{"anthropic", "openai-compat", "openai-responses", "azure-openai-responses", "google"} {
			add(name, defaultModelFor(name, ""), false)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// defaultModelFor resolves the fallback model id for a provider key.
func defaultModelFor(providerName, protocol string) string {
	if builtinProviders[providerName] {
		switch providerName {
		case "openai", "openai-compat":
			protocol = "openai-compat"
		case "azure":
			protocol = "azure-openai-responses"
		case "gemini":
			protocol = "google"
		default:
			protocol = providerName
		}
	} else if protocol == "" {
		protocol = "openai-compat"
	}
	return protocolDefaults[normalizeProtocol(protocol)].model
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
