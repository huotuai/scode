// Package mcp bridges external Model Context Protocol servers into the
// agent's tool registry, mirroring deepseek-harness's mcp-client design:
// the official Go SDK owns the wire protocol; this package owns config,
// the reconnect supervisor, tool bridging, and content projection.
//
// Servers are configured in mcp.json at the config directory (NOT
// settings.json):
//
//	{
//	  "mcpServers": {
//	    "github": {"transport": "stdio", "command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"], "env": {"GITHUB_TOKEN": "..."}},
//	    "web": {"transport": "streamable-http", "url": "http://localhost:3000/mcp", "headers": {"Authorization": "Bearer ..."}}
//	  }
//	}
//
// Tools register under server-qualified public names
// (mcp__<server>__<raw>), discovered at startup; mid-session tool-list
// changes arrive as transcript system deltas (ToolsAdded/ToolsRemoved),
// so the prompt prefix stays byte-stable (dsh's generation-swap, adapted
// to scode's transcript model).
package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// Defaults (dsh's Config schema values).
const (
	DefaultToolCallTimeoutMs  = 60_000
	DefaultMaxInstructionByte = 32_768

	DefaultReconnectInitialDelayMs = 500
	DefaultReconnectMaxDelayMs     = 30_000
	DefaultReconnectMaxAttempts    = 10
)

// ServerConfig is one MCP server entry (dsh's StdioConfig |
// StreamableHttpConfig).
type ServerConfig struct {
	Transport string `json:"transport"` // "stdio" | "streamable-http"

	// Enabled toggles the server without losing its config: disabled
	// entries stay in mcp.json but never start (nil = enabled, the
	// default). Toggling at runtime starts/stops the supervisor.
	Enabled *bool `json:"enabled,omitempty"`

	// stdio
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	CWD     string            `json:"cwd,omitempty"`

	// streamable-http
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	// Shared knobs.
	ToolCallTimeoutMs  int              `json:"toolCallTimeoutMs,omitempty"`  // default 60000
	FailOnStartupError bool             `json:"failOnStartupError,omitempty"` // default false
	MaxInstructionByte int              `json:"maxInstructionBytes,omitempty"`
	Reconnect          *ReconnectConfig `json:"reconnect,omitempty"`

	// DefaultPolicy is the permission verdict for this server's tools
	// ("allow"|"ask"|"deny", default "ask"): it expands to a
	// mcp__<server>__* rule in the permission engine, where explicit
	// settings rules still take precedence per-list.
	DefaultPolicy string `json:"defaultPolicy,omitempty"`
}

// ReconnectConfig is dsh's reconnect policy: bounded exponential backoff
// per outage; the attempt budget resets after a stable connection.
type ReconnectConfig struct {
	Enabled        *bool `json:"enabled,omitempty"`        // default true
	InitialDelayMs int   `json:"initialDelayMs,omitempty"` // default 500
	MaxDelayMs     int   `json:"maxDelayMs,omitempty"`     // default 30000 (also the stability window)
	MaxAttempts    int   `json:"maxAttempts,omitempty"`    // default 10
}

// File is the mcp.json root: a map of server name → config.
type File struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}

// serverNamePattern is dsh's SERVER_NAME_PATTERN (kept under the public
// tool-name budget).
var serverNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// LoadFile reads mcp.json; a missing file means no servers.
func LoadFile(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &File{}, nil
		}
		return nil, err
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("mcp.json: %w", err)
	}
	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("mcp.json: %w", err)
	}
	return &f, nil
}

// Validate checks every server entry (dsh's schema rules).
func (f *File) Validate() error {
	for name, cfg := range f.MCPServers {
		if !serverNamePattern.MatchString(name) {
			return fmt.Errorf("server %q: name must match [A-Za-z0-9_-]{1,32}", name)
		}
		switch cfg.Transport {
		case "stdio":
			if cfg.Command == "" {
				return fmt.Errorf("server %q: stdio transport requires command", name)
			}
		case "streamable-http":
			if cfg.URL == "" {
				return fmt.Errorf("server %q: streamable-http transport requires url", name)
			}
		case "":
			return fmt.Errorf("server %q: transport is required (stdio | streamable-http)", name)
		default:
			return fmt.Errorf("server %q: unknown transport %q (stdio | streamable-http)", name, cfg.Transport)
		}
		if cfg.MaxInstructionByte < 0 {
			return fmt.Errorf("server %q: maxInstructionBytes must be positive", name)
		}
	}
	return nil
}

// toolCallTimeout resolves the per-call timeout with dsh's default.
func (c *ServerConfig) toolCallTimeout() int {
	if c.ToolCallTimeoutMs > 0 {
		return c.ToolCallTimeoutMs
	}
	return DefaultToolCallTimeoutMs
}

// IsEnabled reports whether the entry should run (nil = enabled).
func (c *ServerConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// maxInstructions resolves the instructions byte cap with dsh's default.
func (c *ServerConfig) maxInstructions() int {
	if c.MaxInstructionByte > 0 {
		return c.MaxInstructionByte
	}
	return DefaultMaxInstructionByte
}

// reconnectPolicy resolves the reconnect config with dsh's defaults and
// validation (dsh's resolveReconnectPolicy).
func (c *ServerConfig) reconnectPolicy() (reconnectPolicy, error) {
	p := reconnectPolicy{
		enabled:        true,
		initialDelayMs: DefaultReconnectInitialDelayMs,
		maxDelayMs:     DefaultReconnectMaxDelayMs,
		maxAttempts:    DefaultReconnectMaxAttempts,
	}
	if c.Reconnect != nil {
		if c.Reconnect.Enabled != nil {
			p.enabled = *c.Reconnect.Enabled
		}
		if c.Reconnect.InitialDelayMs > 0 {
			p.initialDelayMs = c.Reconnect.InitialDelayMs
		}
		if c.Reconnect.MaxDelayMs > 0 {
			p.maxDelayMs = c.Reconnect.MaxDelayMs
		}
		if c.Reconnect.MaxAttempts > 0 {
			p.maxAttempts = c.Reconnect.MaxAttempts
		}
	}
	if p.initialDelayMs > p.maxDelayMs {
		return p, fmt.Errorf("reconnect.initialDelayMs must be <= maxDelayMs")
	}
	return p, nil
}

type reconnectPolicy struct {
	enabled        bool
	initialDelayMs int
	maxDelayMs     int
	maxAttempts    int
}

// UpsertServer writes one server entry into mcp.json, preserving every
// other key (map-level read-modify-write, the settings.json pattern).
// The entry is stored as plain JSON, so unknown SUB-keys of other
// servers survive; the written entry itself is the typed config.
func UpsertServer(path, name string, cfg ServerConfig) error {
	if !serverNamePattern.MatchString(name) {
		return fmt.Errorf("server %q: name must match [A-Za-z0-9_-]{1,32}", name)
	}
	doc, err := readDoc(path)
	if err != nil {
		return err
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
		doc["mcpServers"] = servers
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	var entry map[string]any
	if err := json.Unmarshal(raw, &entry); err != nil {
		return err
	}
	servers[name] = entry
	return writeDoc(path, doc)
}

// DeleteServer removes one server entry from mcp.json, preserving
// every other key.
func DeleteServer(path, name string) error {
	doc, err := readDoc(path)
	if err != nil {
		return err
	}
	if servers, _ := doc["mcpServers"].(map[string]any); servers != nil {
		delete(servers, name)
	}
	return writeDoc(path, doc)
}

// readDoc loads mcp.json as a raw map (missing file = empty doc).
func readDoc(path string) (map[string]any, error) {
	doc := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return doc, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("mcp.json: %w", err)
	}
	return doc, nil
}

// writeDoc persists the raw doc, creating the directory on demand.
func writeDoc(path string, doc map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
