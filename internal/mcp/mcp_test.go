package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"scode/internal/agent"
	"scode/internal/llm"
)

// startTestServer runs an in-process MCP server with one "echo" tool on
// one end of an in-memory pipe, returning the client-end transport
// factory and the server session for teardown.
func startTestServer(t *testing.T, instructions string) (factory func() (mcpsdk.Transport, error), cleanup func()) {
	t.Helper()
	clientT, serverT := mcpsdk.NewInMemoryTransports()

	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test-server", Version: "0.0.1"}, nil)
	if instructions != "" {
		// ServerOptions.Instructions is not a field in this SDK version;
		// instructions travel via the initialize result — set through
		// ServerOptions below if present, else skip.
	}
	server.AddTool(&mcpsdk.Tool{
		Name:        "echo",
		Description: "echo the input text",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
	}, func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		var args struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &args)
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "echo: " + args.Text}},
		}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return func() (mcpsdk.Transport, error) { return clientT, nil }, func() {
		ss.Close() //nolint:errcheck
		cancel()
	}
}

func testHooks() (Hooks, *hookLog) {
	log := &hookLog{}
	return Hooks{
		ToolsChanged:        func(server string, added []agent.Tool, removed []string) { log.record(server, added, removed) },
		InstructionsChanged: func(server, text string) { log.recordInstructions(server, text) },
		Logf:                func(string, ...any) {},
	}, log
}

type hookLog struct {
	mu           sync.Mutex
	calls        int
	added        map[string][]string
	removed      map[string][]string
	instructions map[string]string
}

func (h *hookLog) record(server string, added []agent.Tool, removed []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	if h.added == nil {
		h.added = map[string][]string{}
		h.removed = map[string][]string{}
	}
	var names []string
	for _, t := range added {
		names = append(names, t.Decl().Name)
	}
	h.added[server] = names
	h.removed[server] = removed
}

func (h *hookLog) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func (h *hookLog) recordInstructions(server, text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.instructions == nil {
		h.instructions = map[string]string{}
	}
	h.instructions[server] = text
}

func (h *hookLog) snapshot(server string) (added, removed []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.added[server], h.removed[server]
}

func TestSupervisorConnectSyncAndCall(t *testing.T) {
	factory, cleanup := startTestServer(t, "")
	defer cleanup()

	hooks, log := testHooks()
	sup, err := newSupervisor("test", &ServerConfig{Transport: "stdio", Command: "unused"}, hooks)
	if err != nil {
		t.Fatal(err)
	}
	sup.transportFactory = factory
	sup.start()
	defer sup.Shutdown()

	if err := sup.AwaitReady(); err != nil {
		t.Fatal(err)
	}
	// The generation registered under the public name.
	added, _ := log.snapshot("test")
	if len(added) != 1 || added[0] != "mcp__test__echo" {
		t.Fatalf("added = %v", added)
	}
	tools := sup.Tools()
	if len(tools) != 1 {
		t.Fatalf("tools = %+v", tools)
	}
	decl := tools[0].Decl()
	if decl.Description != "echo the input text" || len(decl.Parameters) == 0 {
		t.Fatalf("decl = %+v", decl)
	}
	// Execute through the live session.
	res := tools[0].Execute(agent.ToolContext{Ctx: context.Background()}, json.RawMessage(`{"text":"hi"}`))
	if res.IsError || len(res.Content) != 1 || res.Content[0].Text != "echo: hi" {
		t.Fatalf("result = %+v", res)
	}
}

func TestSupervisorReconnectAndGiveUp(t *testing.T) {
	// A factory that always fails: the supervisor burns its attempt
	// budget, then unregisters (dsh's exhaustion path).
	failures := 0
	hooks, _ := testHooks()
	cfg := &ServerConfig{
		Transport: "streamable-http", URL: "http://127.0.0.1:1/unreachable",
		Reconnect: &ReconnectConfig{InitialDelayMs: 1, MaxDelayMs: 2, MaxAttempts: 3},
	}
	sup, err := newSupervisor("dead", cfg, hooks)
	if err != nil {
		t.Fatal(err)
	}
	sup.transportFactory = func() (mcpsdk.Transport, error) {
		failures++
		clientT, _ := mcpsdk.NewInMemoryTransports()
		return clientT, nil // connects to nothing: the handshake times out... use a failing transport instead
	}
	// Use a straight failing factory: transport creation itself fails.
	sup.transportFactory = func() (mcpsdk.Transport, error) {
		failures++
		return nil, context.DeadlineExceeded
	}
	sup.start()
	defer sup.Shutdown()
	if err := sup.AwaitReady(); err == nil {
		t.Fatal("ready must report the first attempt's failure")
	}
	// Wait out the reconnect budget (1ms, 2ms, 2ms delays + attempts).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sup.mu.Lock()
		n := sup.failedAttempts
		sup.mu.Unlock()
		if n > 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	sup.mu.Lock()
	n := sup.failedAttempts
	sup.mu.Unlock()
	if n <= 3 {
		t.Fatalf("failedAttempts = %d, want budget exhausted", n)
	}
	if failures < 4 {
		t.Fatalf("attempts = %d, want initial + 3 retries", failures)
	}
}

func TestSupervisorGiveUpUnregisters(t *testing.T) {
	factory, cleanup := startTestServer(t, "")
	defer cleanup()

	hooks, log := testHooks()
	cfg := &ServerConfig{
		Transport: "stdio", Command: "unused",
		Reconnect: &ReconnectConfig{InitialDelayMs: 1, MaxDelayMs: 2, MaxAttempts: 1},
	}
	sup, err := newSupervisor("test", cfg, hooks)
	if err != nil {
		t.Fatal(err)
	}
	// First generation works; after the drop the factory fails.
	first := true
	sup.transportFactory = func() (mcpsdk.Transport, error) {
		if first {
			first = false
			return factory()
		}
		return nil, context.DeadlineExceeded
	}
	sup.start()
	defer sup.Shutdown()
	if err := sup.AwaitReady(); err != nil {
		t.Fatal(err)
	}
	if len(sup.Tools()) != 1 {
		t.Fatal("want one tool before the drop")
	}
	// Kill the server end: the watcher fires generationDown, the
	// (single-attempt) budget exhausts, tools unregister.
	cleanup()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, removed := log.snapshot("test")
		if len(removed) == 1 && removed[0] == "mcp__test__echo" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, removed := log.snapshot("test")
	t.Fatalf("removed = %v, want the generation unregistered after give-up", removed)
}

func TestBridgeToolDisconnected(t *testing.T) {
	hooks, _ := testHooks()
	sup, err := newSupervisor("x", &ServerConfig{Transport: "stdio", Command: "x"}, hooks)
	if err != nil {
		t.Fatal(err)
	}
	tool := &bridgeTool{
		decl:    llm.Tool{Name: "mcp__x__y"},
		rawName: "y",
		sup:     sup,
	}
	res := tool.Execute(agent.ToolContext{Ctx: context.Background()}, json.RawMessage(`{}`))
	if !res.IsError {
		t.Fatal("disconnected server must produce an error result")
	}
}

// startTestServerGen returns a factory where EACH call spawns a fresh
// in-process server on a new pipe (for reconnect tests), plus a kill
// func ending every spawned server.
func startTestServerGen(t *testing.T) (factory func() (mcpsdk.Transport, error), kill func()) {
	t.Helper()
	var mu sync.Mutex
	var cleanups []func()
	factory = func() (mcpsdk.Transport, error) {
		f, cleanup := startTestServer(t, "")
		mu.Lock()
		cleanups = append(cleanups, cleanup)
		mu.Unlock()
		return f()
	}
	kill = func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range cleanups {
			c()
		}
		cleanups = nil
	}
	t.Cleanup(kill)
	return factory, kill
}

// Reconnect with an IDENTICAL tool list must not append a transcript
// delta (cache-prefix hygiene): the hook fires only for real changes.
func TestSupervisorReconnectIdenticalNoDelta(t *testing.T) {
	factory, kill := startTestServerGen(t)

	hooks, log := testHooks()
	cfg := &ServerConfig{
		Transport: "stdio", Command: "unused",
		Reconnect: &ReconnectConfig{InitialDelayMs: 1, MaxDelayMs: 5, MaxAttempts: 10},
	}
	sup, err := newSupervisor("test", cfg, hooks)
	if err != nil {
		t.Fatal(err)
	}
	sup.transportFactory = factory
	sup.start()
	defer sup.Shutdown()
	if err := sup.AwaitReady(); err != nil {
		t.Fatal(err)
	}
	if got := log.callCount(); got != 1 {
		t.Fatalf("initial sync hook calls = %d, want 1", got)
	}

	// Kill the current server; the supervisor reconnects to a fresh one
	// serving the SAME tool list.
	kill()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sup.mu.Lock()
		up := !sup.connectedAt.IsZero()
		sup.mu.Unlock()
		if up {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Give the (skipped) hook a chance to fire erroneously.
	time.Sleep(100 * time.Millisecond)
	if got := log.callCount(); got != 1 {
		t.Fatalf("hook calls after identical re-sync = %d, want 1 (delta-free)", got)
	}
	if len(sup.Tools()) != 1 {
		t.Fatalf("tools after reconnect = %+v", sup.Tools())
	}
}

// blockingFactory gates a transport factory on a release channel: the
// connection attempt parks inside createTransport until the channel
// closes (a still-connecting server for startup-wait tests).
func blockingFactory(release <-chan struct{}, next func() (mcpsdk.Transport, error)) func() (mcpsdk.Transport, error) {
	return func() (mcpsdk.Transport, error) {
		<-release
		return next()
	}
}

// WaitReadyTimeout collects settled servers within one absolute budget
// and lists the rest pending (pi's startupWaitMs): the wait never
// exceeds the budget, and late servers settle afterwards normally.
func TestWaitReadyTimeout(t *testing.T) {
	f := &File{MCPServers: map[string]ServerConfig{
		"fast": {Transport: "stdio", Command: "unused"},
		"slow": {Transport: "stdio", Command: "unused"},
	}}
	hooks, _ := testHooks()
	mgr, err := NewManager(f, hooks)
	if err != nil {
		t.Fatal(err)
	}
	fastFactory, cleanupFast := startTestServer(t, "")
	t.Cleanup(cleanupFast)
	slowFactory, cleanupSlow := startTestServer(t, "")
	t.Cleanup(cleanupSlow)
	release := make(chan struct{})
	for _, s := range mgr.sups {
		if s.name == "slow" {
			s.transportFactory = blockingFactory(release, slowFactory)
		} else {
			s.transportFactory = fastFactory
		}
	}

	// Zero budget on unstarted supervisors: everything pending, no wait.
	ready, pending := mgr.WaitReadyTimeout(0)
	if len(ready) != 0 || len(pending) != 2 {
		t.Fatalf("pre-start: ready = %v, pending = %v", ready, pending)
	}

	mgr.Start()
	start := time.Now()
	ready, pending = mgr.WaitReadyTimeout(300 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("bounded wait took %v", elapsed)
	}
	if err := ready["fast"]; err != nil {
		t.Fatalf("fast server: ready err = %v", err)
	}
	if _, ok := ready["slow"]; ok {
		t.Fatalf("slow server should still be pending, ready = %v", ready)
	}
	if len(pending) != 1 || pending[0] != "slow" {
		t.Fatalf("pending = %v, want [slow]", pending)
	}

	// After the gate opens the straggler settles normally.
	close(release)
	all := mgr.WaitReady()
	if err := all["slow"]; err != nil {
		t.Fatalf("slow server after release: %v", err)
	}
	mgr.Shutdown()
}

// WaitReadyFatal awaits only failOnStartupError servers: a failing
// critical server is fatal even while a normal server is still
// connecting; a healthy critical server passes without waiting for the
// rest.
func TestWaitReadyFatal(t *testing.T) {
	hooks, _ := testHooks()
	release := make(chan struct{})

	failing := &File{MCPServers: map[string]ServerConfig{
		"critical": {Transport: "stdio", Command: "unused", FailOnStartupError: true},
		"normal":   {Transport: "stdio", Command: "unused"},
	}}
	mgr, err := NewManager(failing, hooks)
	if err != nil {
		t.Fatal(err)
	}
	factory, cleanup := startTestServer(t, "")
	t.Cleanup(cleanup)
	mgr.SetTransportHook(func(name string) func() (mcpsdk.Transport, error) {
		if name == "critical" {
			return func() (mcpsdk.Transport, error) { return nil, errors.New("boom") }
		}
		return blockingFactory(release, factory)
	})
	for _, s := range mgr.sups {
		s.transportFactory = mgr.transportHook(s.name)
	}
	mgr.Start()
	if err := mgr.WaitReadyFatal(); err == nil || !strings.Contains(err.Error(), "critical") {
		t.Fatalf("WaitReadyFatal = %v, want critical failure", err)
	}
	close(release)
	mgr.Shutdown()

	healthy := &File{MCPServers: map[string]ServerConfig{
		"critical": {Transport: "stdio", Command: "unused", FailOnStartupError: true},
		"normal":   {Transport: "stdio", Command: "unused"},
	}}
	mgr2, err := NewManager(healthy, hooks)
	if err != nil {
		t.Fatal(err)
	}
	release2 := make(chan struct{})
	// Fresh pipe per connection: the critical and normal servers each
	// connect once (sharing one pipe would deadlock the handshake).
	factory2, _ := startTestServerGen(t)
	mgr2.SetTransportHook(func(name string) func() (mcpsdk.Transport, error) {
		if name == "critical" {
			return factory2
		}
		return blockingFactory(release2, factory2)
	})
	for _, s := range mgr2.sups {
		s.transportFactory = mgr2.transportHook(s.name)
	}
	mgr2.Start()
	start := time.Now()
	if err := mgr2.WaitReadyFatal(); err != nil {
		t.Fatalf("WaitReadyFatal = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("WaitReadyFatal waited for non-fatal servers: %v", elapsed)
	}
	close(release2)
	mgr2.Shutdown()
}

// Manager supervisor order is sorted by server name, keeping the initial
// tool declaration byte-deterministic across runs (prefix cache).
func TestManagerSortedServers(t *testing.T) {
	f := &File{MCPServers: map[string]ServerConfig{
		"zeta":  {Transport: "stdio", Command: "x"},
		"alpha": {Transport: "stdio", Command: "x"},
		"mid":   {Transport: "stdio", Command: "x"},
	}}
	hooks, _ := testHooks()
	m, err := NewManager(f, hooks)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range m.sups {
		names = append(names, s.name)
	}
	want := []string{"alpha", "mid", "zeta"}
	if len(names) != 3 || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Fatalf("order = %v, want %v", names, want)
	}
}

func TestProjectContent(t *testing.T) {
	content := []mcpsdk.Content{
		&mcpsdk.TextContent{Text: "line1"},
		&mcpsdk.TextContent{Text: "line2"},
		&mcpsdk.ImageContent{MIMEType: "image/png", Data: []byte{1, 2, 3}},
		&mcpsdk.AudioContent{MIMEType: "audio/wav", Data: []byte{1}},
		&mcpsdk.ResourceLink{Name: "doc", URI: "file:///doc"},
		&mcpsdk.EmbeddedResource{},
	}
	blocks := projectContent(content, "tool")
	// text coalesces, image splits at position, others placeholder.
	if len(blocks) != 3 {
		t.Fatalf("blocks = %+v", blocks)
	}
	if blocks[0].Kind != llm.BlockText || blocks[0].Text != "line1\nline2" {
		t.Fatalf("text run = %+v", blocks[0])
	}
	if blocks[1].Kind != llm.BlockImage || blocks[1].MimeType != "image/png" {
		t.Fatalf("image = %+v", blocks[1])
	}
	if blocks[2].Kind != llm.BlockText || blocks[2].Text != "[audio result unsupported: audio/wav]\nResource link: doc (file:///doc)\n[embedded resource unsupported]" {
		t.Fatalf("tail = %+v", blocks[2])
	}
	// Bad media type: diagnostic text instead of an image block.
	blocks = projectContent([]mcpsdk.Content{&mcpsdk.ImageContent{MIMEType: "image/tiff", Data: []byte{1}}}, "tool")
	if len(blocks) != 1 || blocks[0].Kind != llm.BlockText {
		t.Fatalf("blocks = %+v", blocks)
	}
	// Empty content: explicit no-content marker.
	blocks = projectContent(nil, "tool")
	if len(blocks) != 1 || blocks[0].Text != "(tool returned no model-visible content)" {
		t.Fatalf("blocks = %+v", blocks)
	}
}

// A reconnect that CONNECTS but fails its post-handshake sync
// (failGeneration) must keep the previous generation's tool
// bookkeeping: the app registry still holds those registrations, and
// the give-up path unregisters from s.tools. Nil'ing it on the failed
// re-sync stranded the registry's tools forever.
func TestGiveUpAfterFailedResyncUnregisters(t *testing.T) {
	// Server A: one echo tool — the first generation.
	factoryA, cleanupA := startTestServer(t, "")
	defer cleanupA()
	// Server B: instructions beyond the 32KiB cap — every reconnect
	// handshake succeeds, then failGeneration tears the session down.
	clientB, serverB := mcpsdk.NewInMemoryTransports()
	srvB := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "big", Version: "0"},
		&mcpsdk.ServerOptions{Instructions: strings.Repeat("x", 64*1024)})
	ssB, err := srvB.Connect(context.Background(), serverB, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ssB.Close() //nolint:errcheck

	hooks, log := testHooks()
	cfg := &ServerConfig{
		Transport: "stdio", Command: "unused",
		Reconnect: &ReconnectConfig{InitialDelayMs: 1, MaxDelayMs: 2, MaxAttempts: 3},
	}
	sup, err := newSupervisor("test", cfg, hooks)
	if err != nil {
		t.Fatal(err)
	}
	first := true
	sup.transportFactory = func() (mcpsdk.Transport, error) {
		if first {
			first = false
			return factoryA()
		}
		return clientB, nil
	}
	sup.start()
	defer sup.Shutdown()
	if err := sup.AwaitReady(); err != nil {
		t.Fatal(err)
	}
	if len(sup.Tools()) != 1 {
		t.Fatal("want one tool before the drop")
	}

	// Drop the established generation: reconnects reach server B and
	// fail at the instructions cap; once the budget exhausts, give-up
	// must unregister the still-registered tool.
	sup.mu.Lock()
	cs := sup.cs
	sup.mu.Unlock()
	if cs == nil {
		t.Fatal("no live session")
	}
	cs.Close() //nolint:errcheck
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, removed := log.snapshot("test"); len(removed) == 1 && removed[0] == "mcp__test__echo" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, removed := log.snapshot("test")
	t.Fatalf("give-up removed = %v, want the still-registered tool", removed)
}

// freshEchoTransport builds a NEW in-memory echo server per call, so
// reconnects and restarts always find a fresh peer.
func freshEchoTransport(t *testing.T) func() (mcpsdk.Transport, error) {
	t.Helper()
	return func() (mcpsdk.Transport, error) {
		clientT, serverT := mcpsdk.NewInMemoryTransports()
		server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "fresh", Version: "0"}, nil)
		server.AddTool(&mcpsdk.Tool{
			Name:        "echo",
			Description: "echo the input text",
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

func waitForCond(t *testing.T, what string, probe func() bool) {
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

// Runtime add/remove: Add starts a supervisor whose tools register via
// ToolsChanged; Remove disposes it and unregisters synchronously.
func TestManagerRuntimeAddRemove(t *testing.T) {
	hooks, log := testHooks()
	mgr, err := NewManager(&File{}, hooks)
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetTransportHook(func(string) func() (mcpsdk.Transport, error) { return freshEchoTransport(t) })
	defer mgr.Shutdown()

	if err := mgr.Add("srv", ServerConfig{Transport: "stdio", Command: "unused"}); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, "tools to register", func() bool {
		added, _ := log.snapshot("srv")
		return len(added) == 1 && added[0] == "mcp__srv__echo"
	})
	if st, ok := mgr.Status("srv"); !ok || st.State != StatusConnected || st.Tools != 1 {
		t.Fatalf("status = %+v ok=%v", st, ok)
	}
	if err := mgr.Add("srv", ServerConfig{Transport: "stdio", Command: "x"}); err == nil {
		t.Fatal("duplicate add accepted")
	}

	mgr.Remove("srv")
	waitForCond(t, "removal delta", func() bool {
		_, removed := log.snapshot("srv")
		return len(removed) == 1 && removed[0] == "mcp__srv__echo"
	})
	if _, ok := mgr.Status("srv"); ok {
		t.Fatal("removed server still has status")
	}
	if servers := mgr.Servers(); len(servers) != 0 {
		t.Fatalf("servers = %v", servers)
	}
}

// Restart rebuilds under an edited config: the OLD generation
// unregisters before the new one registers (an overlapping shutdown
// would strip the new generation's same-named tools).
func TestManagerRuntimeRestart(t *testing.T) {
	hooks, log := testHooks()
	mgr, err := NewManager(&File{}, hooks)
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetTransportHook(func(string) func() (mcpsdk.Transport, error) { return freshEchoTransport(t) })
	defer mgr.Shutdown()

	if err := mgr.Restart("srv", ServerConfig{Transport: "stdio", Command: "v1"}); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, "v1 tools", func() bool {
		added, _ := log.snapshot("srv")
		return len(added) == 1 && added[0] == "mcp__srv__echo"
	})
	if err := mgr.Restart("srv", ServerConfig{Transport: "stdio", Command: "v2"}); err != nil {
		t.Fatal(err)
	}
	// The snapshot only keeps the LAST ToolsChanged call, so the proof
	// of correct ordering is the call sequence: v1 registered, v1
	// removed (shutdown), v2 registered.
	waitForCond(t, "v2 re-registration", func() bool {
		return log.callCount() >= 3
	})
	added, _ := log.snapshot("srv")
	if len(added) != 1 || added[0] != "mcp__srv__echo" {
		t.Fatalf("final generation = %v", added)
	}
	if st, ok := mgr.Status("srv"); !ok || st.State != StatusConnected {
		t.Fatalf("status after restart = %+v ok=%v", st, ok)
	}
}

// Disabled entries stay in the file but never start.
func TestNewManagerSkipsDisabled(t *testing.T) {
	off := false
	f := &File{MCPServers: map[string]ServerConfig{
		"off": {Transport: "stdio", Command: "x", Enabled: &off},
		"on":  {Transport: "stdio", Command: "y"},
	}}
	hooks, _ := testHooks()
	mgr, err := NewManager(f, hooks)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown()
	servers := mgr.Servers()
	if len(servers) != 1 || servers[0] != "on" {
		t.Fatalf("servers = %v, want [on]", servers)
	}
}

// The status lifecycle through give-up: a failing transport burns the
// budget and the snapshot reports gave-up.
func TestStatusGivesUp(t *testing.T) {
	hooks, _ := testHooks()
	mgr, err := NewManager(&File{}, hooks)
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetTransportHook(func(string) func() (mcpsdk.Transport, error) {
		return func() (mcpsdk.Transport, error) { return nil, context.DeadlineExceeded }
	})
	defer mgr.Shutdown()
	if err := mgr.Add("dead", ServerConfig{
		Transport: "stdio", Command: "unused",
		Reconnect: &ReconnectConfig{InitialDelayMs: 1, MaxDelayMs: 2, MaxAttempts: 2},
	}); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, "give-up", func() bool {
		st, ok := mgr.Status("dead")
		return ok && st.State == StatusGaveUp
	})
}
