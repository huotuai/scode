package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"scode/internal/agent"
	"scode/internal/llm"
)

// bridgeTool is one MCP tool registered on the agent's registry (dsh's
// createMcpToolDefinition): the declaration uses the public name and the
// server's schema verbatim; execution goes through the supervisor's
// CURRENT session, so reconnections never strand a registered tool.
type bridgeTool struct {
	decl        llm.Tool
	rawName     string
	description string
	sup         *Supervisor
}

func (t *bridgeTool) Decl() llm.Tool { return t.decl }

// Execute invokes the upstream tool (dsh's executor): the call goes out
// with the per-call timeout; MCP isError results become scode error
// results; content projects through the durable-content rules.
func (t *bridgeTool) Execute(tc agent.ToolContext, args json.RawMessage) agent.ToolResult {
	cs, err := t.sup.session()
	if err != nil {
		return agent.ErrorResult(err.Error())
	}
	// A misbehaving model can send a bare string/number/null; fall back
	// to {} so the server produces its own missing-param error (dsh).
	var argsObj any = map[string]any{}
	if len(args) > 0 && json.Valid(args) {
		var v any
		if err := json.Unmarshal(args, &v); err == nil {
			if obj, ok := v.(map[string]any); ok {
				argsObj = obj
			}
		}
	}

	ctx, cancel := context.WithTimeout(tc.Ctx, time.Duration(t.sup.cfg.toolCallTimeout())*time.Millisecond)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: t.rawName, Arguments: argsObj})
	if err != nil {
		return agent.ErrorResult(fmt.Sprintf("MCP tool %q call failed: %v", t.rawName, err))
	}
	if res.IsError {
		return agent.ErrorResult(extractText(res.Content, t.rawName))
	}
	return agent.ToolResult{Content: projectContent(res.Content, t.rawName)}
}

// Manager owns every configured server's supervisor (dsh's plugin
// registry): startup is non-blocking — only failOnStartupError servers
// are awaited (WaitReadyFatal), the rest connect in the background and
// the session's first prompt gives them a bounded wait
// (WaitReadyTimeout, pi's startupWaitMs). Servers can be added,
// removed, and restarted at runtime — Supervisor.Shutdown is one-way,
// so a removed supervisor is disposed and dropped, and re-adding
// builds a fresh one.
type Manager struct {
	mu    sync.Mutex
	sups  []*Supervisor
	hooks Hooks

	// transportHook, when set, overrides createTransport for the named
	// server (test seam for Manager-level add/restart paths).
	transportHook func(name string) func() (mcpsdk.Transport, error)
}

// NewManager builds supervisors for every valid, ENABLED server entry
// (dsh's per-entry plugin instances); disabled entries stay in the file
// but never start. Server order is sorted by name: the initial tool
// declaration must be byte-deterministic across runs, or a resumed
// session's prefix cache misses (Go map iteration is random).
func NewManager(f *File, hooks Hooks) (*Manager, error) {
	m := &Manager{hooks: hooks}
	names := make([]string, 0, len(f.MCPServers))
	for name := range f.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cfg := f.MCPServers[name]
		if !cfg.IsEnabled() {
			continue
		}
		sup, err := m.newSup(name, cfg)
		if err != nil {
			return nil, err
		}
		m.sups = append(m.sups, sup)
	}
	return m, nil
}

// newSup builds one supervisor, applying the transport test seam.
func (m *Manager) newSup(name string, cfg ServerConfig) (*Supervisor, error) {
	sup, err := newSupervisor(name, &cfg, m.hooks)
	if err != nil {
		return nil, err
	}
	if m.transportHook != nil {
		if factory := m.transportHook(name); factory != nil {
			sup.transportFactory = factory
		}
	}
	return sup, nil
}

// TransportHook overrides per-server transport creation (test seam:
// in-memory SDK transports replace real stdio/http servers).
type TransportHook func(name string) func() (mcpsdk.Transport, error)

// SetTransportHook installs a transport factory override: it applies
// to every supervisor this manager builds AND retro-applies to the
// initial supervisors (they read the factory per connection attempt).
// Call before Start.
func (m *Manager) SetTransportHook(hook TransportHook) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.transportHook = hook
	for _, s := range m.sups {
		s.transportFactory = nil
		if hook != nil {
			s.transportFactory = hook(s.name)
		}
	}
}

// snapshot copies the supervisor slice under the lock.
func (m *Manager) snapshot() []*Supervisor {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*Supervisor(nil), m.sups...)
}

// Add starts one server's supervisor at runtime (the caller persists
// the config entry separately). Duplicate names are rejected.
func (m *Manager) Add(name string, cfg ServerConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sups {
		if s.name == name {
			return fmt.Errorf("mcp: server %q already running", name)
		}
	}
	sup, err := m.newSup(name, cfg)
	if err != nil {
		return err
	}
	m.sups = append(m.sups, sup)
	sup.start()
	return nil
}

// Remove disposes one server's supervisor synchronously; its tools
// unregister through the ToolsChanged hook before the call returns.
func (m *Manager) Remove(name string) {
	m.mu.Lock()
	var sup *Supervisor
	kept := make([]*Supervisor, 0, len(m.sups))
	for _, s := range m.sups {
		if s.name == name {
			sup = s
			continue
		}
		kept = append(kept, s)
	}
	m.sups = kept
	m.mu.Unlock()
	if sup != nil {
		sup.Shutdown() // outside the lock: it waits on the supervisor's wg
	}
}

// Restart swaps a server's supervisor for a fresh one under an edited
// config. The OLD supervisor shuts down first, synchronously: a live
// overlap would let its shutdown removal unregister the new
// generation's same-named tools. Unknown-name restarts are just adds.
func (m *Manager) Restart(name string, cfg ServerConfig) error {
	m.Remove(name)
	return m.Add(name, cfg)
}

// Servers lists the running servers' names, sorted.
func (m *Manager) Servers() []string {
	sups := m.snapshot()
	names := make([]string, 0, len(sups))
	for _, s := range sups {
		names = append(names, s.name)
	}
	sort.Strings(names)
	return names
}

// Status returns one running server's live snapshot.
func (m *Manager) Status(name string) (Status, bool) {
	for _, s := range m.snapshot() {
		if s.name == name {
			return s.Status(), true
		}
	}
	return Status{}, false
}

// Start launches every supervisor (non-blocking per server).
func (m *Manager) Start() {
	for _, s := range m.snapshot() {
		s.start()
	}
}

// WaitReady blocks until every server's FIRST attempt settles (dsh's
// startup-await), returning per-server errors. With
// failOnStartupError=false the caller tolerates failures (the
// supervisors are already in their reconnect loops).
func (m *Manager) WaitReady() map[string]error {
	out := map[string]error{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, s := range m.snapshot() {
		wg.Add(1)
		go func(s *Supervisor) {
			defer wg.Done()
			err := s.AwaitReady()
			mu.Lock()
			out[s.name] = err
			mu.Unlock()
		}(s)
	}
	wg.Wait()
	return out
}

// WaitReadyTimeout is WaitReady bounded by one absolute deadline (pi's
// startupWaitMs wait before the first prompt): servers whose first
// attempt settles within the budget return in ready; the rest are
// listed pending (in server order) and keep connecting in the
// background — their tools arrive through the generation hooks when
// they connect. A zero budget collects only already-settled servers.
func (m *Manager) WaitReadyTimeout(d time.Duration) (ready map[string]error, pending []string) {
	ready = map[string]error{}
	timer := time.NewTimer(d)
	defer timer.Stop()
	expired := false
	for _, s := range m.snapshot() {
		if expired {
			select {
			case <-s.readyCh:
				ready[s.name] = s.AwaitReady()
			default:
				pending = append(pending, s.name)
			}
			continue
		}
		select {
		case <-s.readyCh:
			ready[s.name] = s.AwaitReady()
		case <-timer.C:
			expired = true
			pending = append(pending, s.name)
		}
	}
	return ready, pending
}

// WaitReadyFatal blocks until the FIRST attempt of every server with
// failOnStartupError settles and reports the first failure (dsh's
// activation rejection). Servers without the flag are not awaited:
// with the non-blocking startup they connect in the background.
func (m *Manager) WaitReadyFatal() error {
	for _, s := range m.snapshot() {
		if !s.cfg.FailOnStartupError {
			continue
		}
		if err := s.AwaitReady(); err != nil {
			return fmt.Errorf("mcp(%s): initial connection or tool synchronization failed: %w", s.name, err)
		}
	}
	return nil
}

// FatalStartupError reports the first ready error from a server with
// failOnStartupError set (dsh's activation rejection), or nil.
func (m *Manager) FatalStartupError(ready map[string]error) error {
	for _, s := range m.snapshot() {
		if err := ready[s.name]; err != nil && s.cfg.FailOnStartupError {
			return fmt.Errorf("mcp(%s): initial connection or tool synchronization failed: %w", s.name, err)
		}
	}
	return nil
}

// Tools collects every supervisor's current tool generation (for the
// initial declaration after WaitReady).
func (m *Manager) Tools() []agent.Tool {
	var out []agent.Tool
	for _, s := range m.snapshot() {
		out = append(out, s.Tools()...)
	}
	return out
}

// Instructions collects every supervisor's current instructions block,
// keyed by server name.
func (m *Manager) Instructions() map[string]string {
	out := map[string]string{}
	for _, s := range m.snapshot() {
		if text := s.Instructions(); text != "" {
			out[s.name] = text
		}
	}
	return out
}

// Shutdown disposes every supervisor (dsh's plugin disposal).
func (m *Manager) Shutdown() {
	var wg sync.WaitGroup
	for _, s := range m.snapshot() {
		wg.Add(1)
		go func(s *Supervisor) {
			defer wg.Done()
			s.Shutdown()
		}(s)
	}
	wg.Wait()
}
