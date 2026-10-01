package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"scode/internal/agent"
	"scode/internal/llm"
)

// Hooks reports supervisor events to the app layer (which owns the tool
// registry and the transcript).
type Hooks struct {
	// ToolsChanged reports a full generation swap for one server (dsh's
	// phase-2 swap): added carries the complete NEW generation of public
	// tools, removed the public names that left. The swap is all-or-nothing.
	ToolsChanged func(server string, added []agent.Tool, removed []string)
	// InstructionsChanged reports a server's current instructions block
	// ("" when the server is down or gave up).
	InstructionsChanged func(server, text string)
	// Logf receives supervisor diagnostics (dsh's ctx.logger).
	Logf func(format string, args ...any)
}

// Supervisor owns one MCP server's connection generations (dsh's
// connection supervisor): initial connect + tool sync, reconnect with
// bounded exponential backoff per outage, give-up after the attempt
// budget, and full tool unregistration on disposal.
type Supervisor struct {
	name   string
	cfg    *ServerConfig
	policy reconnectPolicy
	hooks  Hooks

	mu             sync.Mutex
	cs             *mcpsdk.ClientSession // current generation; nil while down
	tools          []agent.Tool          // current generation's registrations
	instructions   string
	connectedAt    time.Time // zero while down
	failedAttempts int
	readyFired     bool // the first attempt settled (Ready already delivered)
	gaveUp         bool // the reconnect budget is exhausted (terminal until restart)
	disposed       bool
	readyOnce      sync.Once
	readyCh        chan error
	done           chan struct{}
	wg             sync.WaitGroup

	// transportFactory overrides createTransport (tests inject in-memory
	// transports).
	transportFactory func() (mcpsdk.Transport, error)
}

func newSupervisor(name string, cfg *ServerConfig, hooks Hooks) (*Supervisor, error) {
	policy, err := cfg.reconnectPolicy()
	if err != nil {
		return nil, fmt.Errorf("mcp(%s): %w", name, err)
	}
	return &Supervisor{
		name:    name,
		cfg:     cfg,
		policy:  policy,
		hooks:   hooks,
		readyCh: make(chan error, 1),
		done:    make(chan struct{}),
	}, nil
}

// log emits a supervisor diagnostic (dsh's label prefix).
func (s *Supervisor) log(format string, args ...any) {
	if s.hooks.Logf != nil {
		s.hooks.Logf("mcp(%s): "+format, append([]any{s.name}, args...)...)
	}
}

// Ready settles after the FIRST connection attempt completes, success or
// failure (dsh's connection.ready): the error is nil on success.
func (s *Supervisor) Ready() <-chan error { return s.readyCh }

// Instructions returns the current attributed instructions block.
func (s *Supervisor) Instructions() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.instructions
}

// Status states for the Status snapshot (UI surfaces like the /mcp
// overlay).
const (
	StatusConnecting   = "connecting"   // first attempt in flight
	StatusConnected    = "connected"    // a generation is live
	StatusReconnecting = "reconnecting" // established once, now between attempts
	StatusGaveUp       = "gave-up"      // attempt budget exhausted, tools unregistered
)

// Status is a point-in-time supervisor snapshot.
type Status struct {
	State                 string
	Tools                 int
	Attempts, MaxAttempts int
}

// Status returns the supervisor's live snapshot (connecting → connected
// → reconnecting → gave-up; disposed supervisors are gone from the
// manager, so no disposed state exists).
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Tools: len(s.tools), Attempts: s.failedAttempts, MaxAttempts: s.policy.maxAttempts}
	switch {
	case s.cs != nil:
		st.State = StatusConnected
	case s.gaveUp:
		st.State = StatusGaveUp
	case s.readyFired:
		st.State = StatusReconnecting
	default:
		st.State = StatusConnecting
	}
	return st
}

// Tools returns the current generation's tools (post-ready).
func (s *Supervisor) Tools() []agent.Tool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.Tool(nil), s.tools...)
}

// start launches the supervisor: the initial attempt runs now; reconnect
// loops as needed. Never blocks.
func (s *Supervisor) start() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		err := s.connectGeneration()
		s.settleReady(err)
		if err != nil {
			s.afterFailedGeneration()
		}
	}()
}

func (s *Supervisor) settleReady(err error) {
	s.readyOnce.Do(func() {
		s.mu.Lock()
		s.readyFired = true
		s.mu.Unlock()
		s.readyCh <- err
	})
}

// connectGeneration makes ONE connection attempt (dsh's
// connectGeneration): fresh transport + client, connect, instructions,
// tool sync, then watch for the drop. Returns the attempt error; the
// watcher owns the post-connect lifecycle.
func (s *Supervisor) connectGeneration() error {
	client := mcpsdk.NewClient(
		&mcpsdk.Implementation{Name: "scode-mcp", Version: "0.1.1"},
		&mcpsdk.ClientOptions{
			ToolListChangedHandler: func(context.Context, *mcpsdk.ToolListChangedRequest) {
				s.onToolListChanged()
			},
		},
	)

	transport, err := s.createTransport()
	if err != nil {
		return err
	}
	// The handshake gets the SDK-default-shaped 60s budget (dsh inherits
	// the SDK's request default); the session itself is unbounded.
	connectCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	cs, err := client.Connect(connectCtx, transport, nil)
	cancel()
	if err != nil {
		return err
	}

	s.mu.Lock()
	if s.disposed {
		s.mu.Unlock()
		cs.Close() //nolint:errcheck
		return context.Canceled
	}
	s.cs = cs
	s.mu.Unlock()

	// Instructions: attributed, byte-capped (dsh).
	init := cs.InitializeResult()
	text := ""
	if init != nil && init.Instructions != "" {
		text = "### MCP server: " + s.name + "\n\n" + init.Instructions
		if len(text) > s.cfg.maxInstructions() {
			s.failGeneration(cs)
			return fmt.Errorf("server instructions exceed maxInstructionBytes (%d)", s.cfg.maxInstructions())
		}
	}

	if err := s.syncTools(cs); err != nil {
		s.failGeneration(cs)
		return err
	}

	s.mu.Lock()
	instructionsChanged := s.instructions != text
	s.instructions = text
	s.connectedAt = time.Now()
	s.gaveUp = false // a live generation ends any give-up state
	s.mu.Unlock()
	if instructionsChanged {
		s.hooks.InstructionsChanged(s.name, text)
	}

	// Watch the session: when it ends (crash, close, network), the
	// generation is down and the reconnect policy takes over.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		cs.Wait() //nolint:errcheck
		s.generationDown(cs)
	}()
	return nil
}

// createTransport builds the per-config transport (dsh's createTransport).
func (s *Supervisor) createTransport() (mcpsdk.Transport, error) {
	if s.transportFactory != nil {
		return s.transportFactory()
	}
	switch s.cfg.Transport {
	case "stdio":
		cmd := exec.Command(s.cfg.Command, s.cfg.Args...)
		cmd.Env = childEnv(s.cfg.Env)
		if s.cfg.CWD != "" {
			cmd.Dir = s.cfg.CWD
		}
		return &mcpsdk.CommandTransport{Command: cmd}, nil
	case "streamable-http":
		return &mcpsdk.StreamableClientTransport{
			Endpoint: s.cfg.URL,
			HTTPClient: &http.Client{
				Transport: &headerRoundTripper{base: http.DefaultTransport, headers: s.cfg.Headers},
			},
		}, nil
	}
	return nil, fmt.Errorf("unknown transport %q", s.cfg.Transport)
}

// headerRoundTripper attaches config headers to every MCP request (dsh's
// requestInit headers).
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for k, v := range h.headers {
		clone.Header.Set(k, v)
	}
	return h.base.RoundTrip(clone)
}

// syncTools fetches and swaps the full tool generation (dsh's syncTools):
// build the next generation first (any failure leaves the previous one
// registered), then swap all-or-nothing via the hook.
func (s *Supervisor) syncTools(cs *mcpsdk.ClientSession) error {
	var mcpTools []*mcpsdk.Tool
	init := cs.InitializeResult()
	if init != nil && init.Capabilities != nil && init.Capabilities.Tools != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		res, err := cs.ListTools(ctx, nil)
		cancel()
		if err != nil {
			return fmt.Errorf("tools/list: %w", err)
		}
		mcpTools = res.Tools
	}

	generation := make([]agent.Tool, 0, len(mcpTools))
	seen := map[string]bool{}
	for _, t := range mcpTools {
		public := PublicName(s.name, t.Name)
		if seen[public] {
			return fmt.Errorf("server listed tool %q more than once — invalid tool list", t.Name)
		}
		seen[public] = true
		generation = append(generation, &bridgeTool{
			decl: llm.Tool{
				Name:        public,
				Description: t.Description,
				Parameters:  marshalSchema(t.InputSchema),
			},
			rawName:     t.Name,
			description: t.Description,
			sup:         s,
		})
	}

	s.mu.Lock()
	previous := s.tools
	s.tools = generation
	identical := toolSetsEqual(previous, generation)
	s.mu.Unlock()
	if identical {
		return nil // no-op re-sync (e.g. reconnect with an unchanged list): keep the transcript delta-free
	}

	removed := make([]string, 0, len(previous))
	for _, old := range previous {
		if !seen[old.Decl().Name] {
			removed = append(removed, old.Decl().Name)
		}
	}
	s.hooks.ToolsChanged(s.name, generation, removed)
	return nil
}

// toolSetsEqual reports whether two generations declare the same set of
// tools (name set + declaration bytes; order-insensitive — CurrentTools
// replays re-additions at their original position anyway, so a reorder
// alone changes nothing on the wire).
func toolSetsEqual(a, b []agent.Tool) bool {
	if len(a) != len(b) {
		return false
	}
	decls := make(map[string]llm.Tool, len(a))
	for _, t := range a {
		decls[t.Decl().Name] = t.Decl()
	}
	for _, t := range b {
		old, ok := decls[t.Decl().Name]
		if !ok || !llm.DeclarationsEqual(old, t.Decl()) {
			return false
		}
	}
	return true
}

// onToolListChanged re-syncs when the server announces a tool-list change
// (dsh's listChanged.onChanged).
func (s *Supervisor) onToolListChanged() {
	s.mu.Lock()
	cs := s.cs
	disposed := s.disposed
	s.mu.Unlock()
	if cs == nil || disposed {
		return
	}
	s.log("tool list changed, re-syncing")
	if err := s.syncTools(cs); err != nil {
		s.log("tool re-sync failed: %v", err)
	}
}

// generationDown handles the loss of an established generation (dsh's
// generationDown + scheduleReconnect). The tool REGISTRATIONS stay in
// place — calls fail with "disconnected" until a successful re-sync
// swaps them or the exhausted budget unregisters them (dsh keeps
// disposers live across outages).
func (s *Supervisor) generationDown(cs *mcpsdk.ClientSession) {
	s.mu.Lock()
	if s.disposed || s.cs != cs {
		s.mu.Unlock()
		return // stale watcher or already replaced
	}
	s.cs = nil
	s.mu.Unlock()
	s.scheduleReconnect(true)
}

// failGeneration tears down a just-connected generation whose sync failed.
// The tool bookkeeping STAYS: registrations from the previous generation
// are still live in the app registry (generationDown keeps them across
// outages), and both the give-up path and the next successful syncTools
// diff against s.tools to compute removals — nil'ing it here would
// strand the registry's tools forever.
func (s *Supervisor) failGeneration(cs *mcpsdk.ClientSession) {
	s.mu.Lock()
	if s.cs == cs {
		s.cs = nil
	}
	s.mu.Unlock()
	cs.Close() //nolint:errcheck
}

// afterFailedGeneration decides the next step after the initial or a
// reconnect attempt failed (dsh's settleFailedGeneration →
// scheduleReconnect with no established connection).
func (s *Supervisor) afterFailedGeneration() {
	s.mu.Lock()
	disposed := s.disposed
	s.mu.Unlock()
	if !disposed {
		s.scheduleReconnect(false)
	}
}

// scheduleReconnect is dsh's reconnect policy: one outage shares one
// attempt budget; a connection stable past maxDelay resets the budget;
// exhaustion unregisters the server's tools and stops.
func (s *Supervisor) scheduleReconnect(lostEstablished bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disposed {
		return
	}
	if !s.policy.enabled {
		if lostEstablished {
			s.log("connection lost and reconnect is disabled — registered tools will fail until restart")
		} else {
			s.log("connection failed and reconnect is disabled — no tools were registered")
		}
		return
	}
	if !s.connectedAt.IsZero() && time.Since(s.connectedAt) >= time.Duration(s.policy.maxDelayMs)*time.Millisecond {
		s.failedAttempts = 0 // stable connection ended the previous outage (dsh)
	}
	s.connectedAt = time.Time{}
	s.failedAttempts++
	if s.failedAttempts > s.policy.maxAttempts {
		s.log("giving up after %d consecutive failed reconnect attempts — tools unregistered", s.policy.maxAttempts)
		names := make([]string, 0, len(s.tools))
		for _, t := range s.tools {
			names = append(names, t.Decl().Name)
		}
		s.tools = nil
		s.instructions = ""
		s.gaveUp = true
		go func() {
			s.hooks.ToolsChanged(s.name, nil, names)
			s.hooks.InstructionsChanged(s.name, "")
		}()
		return
	}
	delayMs := s.policy.initialDelayMs << (s.failedAttempts - 1)
	if delayMs > s.policy.maxDelayMs {
		delayMs = s.policy.maxDelayMs
	}
	action := "connection failed; retrying"
	if lostEstablished {
		action = "connection lost; reconnecting"
	}
	s.log("%s in %dms (attempt %d/%d)", action, delayMs, s.failedAttempts, s.policy.maxAttempts)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		timer := time.NewTimer(time.Duration(delayMs) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-s.done:
			return
		case <-timer.C:
		}
		err := s.connectGeneration()
		if err != nil {
			s.log("connection attempt failed: %v", err)
			s.afterFailedGeneration()
		}
	}()
}

// currentToolNames snapshots the live generation's public names.
func (s *Supervisor) currentToolNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, t.Decl().Name)
	}
	return out
}

// session returns the current generation's session for a tool call
// (dsh's disconnected check).
func (s *Supervisor) session() (*mcpsdk.ClientSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cs == nil {
		return nil, fmt.Errorf("MCP server %q is disconnected", s.name)
	}
	return s.cs, nil
}

// Shutdown stops reconnection, closes the session, and unregisters all
// tools (dsh's dispose).
func (s *Supervisor) Shutdown() {
	s.mu.Lock()
	if s.disposed {
		s.mu.Unlock()
		return
	}
	s.disposed = true
	cs := s.cs
	s.cs = nil
	s.mu.Unlock()
	close(s.done)
	if cs != nil {
		cs.Close() //nolint:errcheck
	}
	s.wg.Wait()
	names := s.currentToolNames()
	if len(names) > 0 {
		s.hooks.ToolsChanged(s.name, nil, names)
	}
}

// marshalSchema renders the server's inputSchema as raw JSON for the
// tool declaration (schema passes through verbatim).
func marshalSchema(schema any) json.RawMessage {
	b, err := json.Marshal(schema)
	if err != nil {
		return nil
	}
	return b
}
