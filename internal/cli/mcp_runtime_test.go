package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"scode/internal/mcp"
)

// The farewell hint names the session id and the resume invocation.
func TestResumeHint(t *testing.T) {
	app := setupMCPApp(t)
	hint := app.ResumeHint()
	id := app.Sess.Header().ID
	if !strings.Contains(hint, id) || !strings.Contains(hint, "--resume "+id) {
		t.Fatalf("hint = %q (id %q)", hint, id)
	}
}

// inMemoryMCP stubs the MCP transport seam: every connection attempt
// gets a fresh in-memory echo server (restarts and reconnects always
// find a live peer).
func inMemoryMCP(t *testing.T) {
	t.Helper()
	orig := mcpTransportHook
	t.Cleanup(func() { mcpTransportHook = orig })
	mcpTransportHook = func(string) func() (mcpsdk.Transport, error) {
		return func() (mcpsdk.Transport, error) {
			clientT, serverT := mcpsdk.NewInMemoryTransports()
			server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "echo-srv", Version: "0"}, nil)
			server.AddTool(&mcpsdk.Tool{
				Name:        "echo",
				Description: "echo",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}, func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "echo"}}}, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			if _, err := server.Connect(ctx, serverT, nil); err != nil {
				cancel()
				return nil, err
			}
			_ = cancel // the server lives until process exit; nothing to cancel sooner
			return clientT, nil
		}
	}
}

// setupMCPApp builds an App with an empty mcp.json (the manager stays
// nil until the first runtime add exercises the lazy creation).
func setupMCPApp(t *testing.T) *App {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	cfgDir := t.TempDir()
	proj := t.TempDir()
	settings := map[string]any{
		"defaultProvider": "openai-compat",
		"providers": map[string]any{
			"openai-compat": map[string]any{"apiKey": "k", "baseUrl": srv.URL, "model": "m"},
		},
	}
	sb, _ := json.Marshal(settings)
	os.WriteFile(filepath.Join(cfgDir, "settings.json"), sb, 0o644) //nolint:errcheck
	t.Setenv("SCODE_DIR", cfgDir)
	t.Chdir(proj)
	app, err := Setup(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() }) //nolint:errcheck
	return app
}

func waitForMCP(t *testing.T, what string, probe func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if probe() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for " + what)
}

// Non-blocking startup (pi: the first prompt does not wait for MCP
// servers): Setup returns immediately with a server whose first
// attempt is still in flight, the first-prompt wait is bounded by
// startupWaitMs, and once the server connects its tools register
// through the generation hooks — no restart.
func TestMCPStartupNonBlocking(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	orig := mcpTransportHook
	t.Cleanup(func() { mcpTransportHook = orig })
	mcpTransportHook = func(string) func() (mcpsdk.Transport, error) {
		return func() (mcpsdk.Transport, error) {
			<-release
			clientT, serverT := mcpsdk.NewInMemoryTransports()
			server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "slow-srv", Version: "0"}, nil)
			server.AddTool(&mcpsdk.Tool{
				Name:        "echo",
				Description: "echo",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}, func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "echo"}}}, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			if _, err := server.Connect(ctx, serverT, nil); err != nil {
				cancel()
				return nil, err
			}
			_ = cancel
			return clientT, nil
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	cfgDir := t.TempDir()
	proj := t.TempDir()
	settings := map[string]any{
		"defaultProvider": "openai-compat",
		"providers": map[string]any{
			"openai-compat": map[string]any{"apiKey": "k", "baseUrl": srv.URL, "model": "m"},
		},
	}
	sb, _ := json.Marshal(settings)
	os.WriteFile(filepath.Join(cfgDir, "settings.json"), sb, 0o644)           //nolint:errcheck
	os.WriteFile(filepath.Join(cfgDir, "mcp.json"), []byte(
		`{"mcpServers":{"slow-srv":{"transport":"stdio","command":"unused"}},"startupWaitMs":300}`), 0o644) //nolint:errcheck
	t.Setenv("SCODE_DIR", cfgDir)
	t.Chdir(proj)

	// Setup must not block on the in-flight first attempt (the old
	// WaitReady would park here until the 60s connect budget).
	type setupResult struct {
		app *App
		err error
	}
	done := make(chan setupResult, 1)
	go func() {
		app, err := Setup(Options{})
		done <- setupResult{app, err}
	}()
	var app *App
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatal(res.err)
		}
		app = res.app
	case <-time.After(5 * time.Second):
		t.Fatal("Setup blocked on a still-connecting MCP server")
	}
	t.Cleanup(func() { app.Close() }) //nolint:errcheck
	t.Cleanup(unblock) // runs BEFORE Close: a blocked factory would hang Shutdown
	if _, ok := app.mcpRegistry.Get("mcp__slow-srv__echo"); ok {
		t.Fatal("the tool must not be registered while the server is still connecting")
	}

	// The first-prompt wait is bounded by startupWaitMs (300ms here).
	start := time.Now()
	app.awaitMCPStartup()
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("first-prompt wait exceeded startupWaitMs: %v", elapsed)
	}

	// Once the server connects, its tools register through the hooks.
	unblock()
	waitForMCP(t, "slow-srv tools", func() bool {
		_, ok := app.mcpRegistry.Get("mcp__slow-srv__echo")
		return ok
	})
}

// Runtime add with no startup servers: the file gains the entry, the
// manager is lazily built, and the tools register (registry + a
// transcript ToolsAdded delta) without a restart — the next request
// replays the delta, so the model can call the tool.
func TestMCPAddRuntime(t *testing.T) {
	inMemoryMCP(t)
	app := setupMCPApp(t)
	if servers := app.MCPServers(); len(servers) != 0 {
		t.Fatalf("servers at startup = %+v", servers)
	}

	if err := app.MCPAdd("echo-srv", mcp.ServerConfig{Transport: "stdio", Command: "unused"}); err != nil {
		t.Fatal(err)
	}
	f, err := mcp.LoadFile(filepath.Join(app.CfgDir, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.MCPServers["echo-srv"]; !ok {
		t.Fatalf("mcp.json missing the entry: %+v", f.MCPServers)
	}

	waitForMCP(t, "connection", func() bool {
		servers := app.MCPServers()
		return len(servers) == 1 && servers[0].Status != nil &&
			servers[0].Status.State == mcp.StatusConnected && servers[0].Status.Tools == 1
	})
	if _, ok := app.mcpRegistry.Get("mcp__echo-srv__echo"); !ok {
		t.Fatal("registry missing the bridged tool")
	}
	msgs := app.Tr.Messages()
	last := msgs[len(msgs)-1]
	found := false
	for _, tool := range last.ToolsAdded {
		if tool.Name == "mcp__echo-srv__echo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no ToolsAdded delta on the transcript: %+v", last)
	}
}

// Validation failures never touch the file; duplicates are rejected
// with the entry intact.
func TestMCPAddValidation(t *testing.T) {
	inMemoryMCP(t)
	app := setupMCPApp(t)
	path := filepath.Join(app.CfgDir, "mcp.json")

	if err := app.MCPAdd("echo-srv", mcp.ServerConfig{Transport: "stdio"}); err == nil {
		t.Fatal("missing command accepted")
	}
	if err := app.MCPAdd("echo-srv", mcp.ServerConfig{Transport: "stdio", Command: "c", DefaultPolicy: "nope"}); err == nil {
		t.Fatal("bad defaultPolicy accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("invalid add wrote the file: %v", err)
	}

	if err := app.MCPAdd("echo-srv", mcp.ServerConfig{Transport: "stdio", Command: "c"}); err != nil {
		t.Fatal(err)
	}
	if err := app.MCPAdd("echo-srv", mcp.ServerConfig{Transport: "stdio", Command: "c2"}); err == nil {
		t.Fatal("duplicate add accepted")
	}
	f, _ := mcp.LoadFile(path)
	if got := f.MCPServers["echo-srv"].Command; got != "c" {
		t.Fatalf("duplicate add overwrote the entry: %q", got)
	}
}

// Remove deletes the entry and unregisters the tools (registry plus a
// ToolsRemoved delta).
func TestMCPRemoveRuntime(t *testing.T) {
	inMemoryMCP(t)
	app := setupMCPApp(t)
	if err := app.MCPAdd("echo-srv", mcp.ServerConfig{Transport: "stdio", Command: "c"}); err != nil {
		t.Fatal(err)
	}
	waitForMCP(t, "connection", func() bool {
		_, ok := app.mcpRegistry.Get("mcp__echo-srv__echo")
		return ok
	})
	if err := app.MCPRemove("echo-srv"); err != nil {
		t.Fatal(err)
	}
	f, err := mcp.LoadFile(filepath.Join(app.CfgDir, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.MCPServers) != 0 {
		t.Fatalf("entry survived removal: %+v", f.MCPServers)
	}
	waitForMCP(t, "unregistration", func() bool {
		_, ok := app.mcpRegistry.Get("mcp__echo-srv__echo")
		return !ok
	})
	msgs := app.Tr.Messages()
	last := msgs[len(msgs)-1]
	found := false
	for _, name := range last.ToolsRemoved {
		if name == "mcp__echo-srv__echo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no ToolsRemoved delta: %+v", last)
	}
}

// Toggle is lossless: off keeps the config (enabled=false) and
// disposes the supervisor; on restarts it under the same entry.
func TestMCPToggleRuntime(t *testing.T) {
	inMemoryMCP(t)
	app := setupMCPApp(t)
	if err := app.MCPAdd("echo-srv", mcp.ServerConfig{Transport: "stdio", Command: "c"}); err != nil {
		t.Fatal(err)
	}
	waitForMCP(t, "connection", func() bool {
		_, ok := app.mcpRegistry.Get("mcp__echo-srv__echo")
		return ok
	})

	if on, err := app.MCPToggle("echo-srv"); err != nil || on {
		t.Fatalf("toggle off = %v err=%v", on, err)
	}
	f, err := mcp.LoadFile(filepath.Join(app.CfgDir, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if entry, ok := f.MCPServers["echo-srv"]; !ok || entry.IsEnabled() {
		t.Fatalf("disabled flag not persisted: %+v", entry)
	}
	waitForMCP(t, "shutdown", func() bool {
		_, ok := app.mcpRegistry.Get("mcp__echo-srv__echo")
		return !ok
	})

	if on, err := app.MCPToggle("echo-srv"); err != nil || !on {
		t.Fatalf("toggle on = %v err=%v", on, err)
	}
	waitForMCP(t, "reconnection", func() bool {
		_, ok := app.mcpRegistry.Get("mcp__echo-srv__echo")
		return ok
	})
}
