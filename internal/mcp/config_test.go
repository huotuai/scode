package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFileMissing(t *testing.T) {
	f, err := LoadFile(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.MCPServers) != 0 {
		t.Fatalf("servers = %+v", f.MCPServers)
	}
}

func TestLoadFileValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	os.WriteFile(path, []byte(`{
		"mcpServers": {
			"github": {"transport": "stdio", "command": "npx", "args": ["-y", "server-github"], "env": {"TOKEN": "x"}},
			"web": {"transport": "streamable-http", "url": "http://localhost:3000/mcp", "headers": {"Authorization": "Bearer y"}}
		}
	}`), 0o644) //nolint:errcheck
	f, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.MCPServers) != 2 {
		t.Fatalf("servers = %+v", f.MCPServers)
	}
	gh := f.MCPServers["github"]
	if gh.Command != "npx" || len(gh.Args) != 2 || gh.Env["TOKEN"] != "x" {
		t.Fatalf("github = %+v", gh)
	}
	if gh.toolCallTimeout() != DefaultToolCallTimeoutMs {
		t.Fatalf("timeout default = %d", gh.toolCallTimeout())
	}
	p, err := gh.reconnectPolicy()
	if err != nil || !p.enabled || p.initialDelayMs != 500 || p.maxDelayMs != 30_000 || p.maxAttempts != 10 {
		t.Fatalf("policy = %+v %v", p, err)
	}
}

func TestLoadFileValidation(t *testing.T) {
	cases := map[string]string{
		"bad name":        `{"mcpServers": {"has space": {"transport": "stdio", "command": "x"}}}`,
		"missing command": `{"mcpServers": {"a": {"transport": "stdio"}}}`,
		"missing url":     `{"mcpServers": {"a": {"transport": "streamable-http"}}}`,
		"no transport":    `{"mcpServers": {"a": {"command": "x"}}}`,
		"bad transport":   `{"mcpServers": {"a": {"transport": "sse", "url": "http://x"}}}`,
	}
	for name, body := range cases {
		path := filepath.Join(t.TempDir(), "mcp.json")
		os.WriteFile(path, []byte(body), 0o644) //nolint:errcheck
		if _, err := LoadFile(path); err == nil {
			t.Fatalf("%s: want error", name)
		}
	}
}

func TestReconnectPolicyValidation(t *testing.T) {
	cfg := &ServerConfig{Reconnect: &ReconnectConfig{InitialDelayMs: 100, MaxDelayMs: 50}}
	if _, err := cfg.reconnectPolicy(); err == nil {
		t.Fatal("initial > max must error (dsh's rule)")
	}
	enabled := false
	cfg = &ServerConfig{Reconnect: &ReconnectConfig{Enabled: &enabled, MaxAttempts: 3}}
	p, err := cfg.reconnectPolicy()
	if err != nil || p.enabled || p.maxAttempts != 3 {
		t.Fatalf("policy = %+v %v", p, err)
	}
}

func TestPublicName(t *testing.T) {
	// Clean case: verbatim.
	if got := PublicName("github", "create_issue"); got != "mcp__github__create_issue" {
		t.Fatalf("got = %q", got)
	}
	// Character normalization changes the name: hashed suffix.
	got := PublicName("web", "do.thing")
	if got == "mcp__web__do_thing" || len(got) > 64 {
		t.Fatalf("lossy normalization must hash: %q", got)
	}
	if !strings.HasPrefix(got, "mcp__web__do_thing_") {
		t.Fatalf("got = %q", got)
	}
	// Over-long names truncate to 64 with the hash.
	long := PublicName("srv", strings.Repeat("x", 100))
	if len(long) != 64 {
		t.Fatalf("len = %d, want 64", len(long))
	}
	// Distinct identities never collapse.
	if PublicName("srv", strings.Repeat("x", 100)) == PublicName("srv", strings.Repeat("y", 100)) {
		t.Fatal("hash collision between distinct identities")
	}
}

func TestChildEnv(t *testing.T) {
	t.Setenv("MY_API_KEY", "secret")
	t.Setenv("SCODE_SESSION_ID", "abc")
	t.Setenv("PLAIN_VAR", "keep")
	env := childEnv(map[string]string{"MY_API_KEY": "explicit"})
	keys := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		keys[k] = v
	}
	if keys["PLAIN_VAR"] != "keep" {
		t.Fatal("plain vars must pass through")
	}
	if _, ok := keys["SCODE_SESSION_ID"]; ok {
		t.Fatal("SCODE_* must be scrubbed")
	}
	// Explicit config env survives the scrub (dsh's merge-after rule).
	if keys["MY_API_KEY"] != "explicit" {
		t.Fatal("explicit env must survive the scrub")
	}
}

// UpsertServer/DeleteServer persist entries while preserving unknown
// keys and sibling entries (map-level read-modify-write).
func TestUpsertDeleteServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	os.WriteFile(path, []byte(`{"mcpServers":{"keep":{"transport":"stdio","command":"k"}},"unknownTop":42}`), 0o644) //nolint:errcheck

	if err := UpsertServer(path, "new", ServerConfig{Transport: "streamable-http", URL: "http://x/mcp"}); err != nil {
		t.Fatal(err)
	}
	f, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.MCPServers["new"]; got.Transport != "streamable-http" || got.URL != "http://x/mcp" {
		t.Fatalf("upserted = %+v", got)
	}
	data, _ := os.ReadFile(path)
	for _, want := range []string{`"keep"`, `"unknownTop"`, `"new"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("doc lost %s:\n%s", want, data)
		}
	}

	if err := DeleteServer(path, "new"); err != nil {
		t.Fatal(err)
	}
	f, err = LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := f.MCPServers["new"]; exists {
		t.Fatal("delete left the entry")
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), `"unknownTop"`) || !strings.Contains(string(data), `"keep"`) {
		t.Fatalf("delete lost siblings:\n%s", data)
	}

	if err := UpsertServer(path, "bad name!", ServerConfig{}); err == nil {
		t.Fatal("invalid name accepted")
	}
}

// Disabled entries parse and IsEnabled resolves the nil default.
func TestEnabledFlag(t *testing.T) {
	off := false
	on := true
	if (&ServerConfig{}).IsEnabled() != true {
		t.Fatal("nil Enabled must default to true")
	}
	if (&ServerConfig{Enabled: &off}).IsEnabled() {
		t.Fatal("explicit false must disable")
	}
	if (&ServerConfig{Enabled: &on}).IsEnabled() != true {
		t.Fatal("explicit true must enable")
	}
}
