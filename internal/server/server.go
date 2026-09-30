package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"scode/internal/agent"
	"scode/internal/cli"
	"scode/internal/config"
	"scode/internal/llm"
	"scode/internal/permission"
	"scode/internal/session"
	"scode/internal/tools"
)

// Session is one live session handle: the cli.App plus the run state.
// One run at a time per session (runMu); cancel aborts the in-flight run.
type Session struct {
	App     *cli.App
	runMu   sync.Mutex
	cancel  context.CancelFunc
	running atomic.Bool
}

// Server hosts sessions behind the JSON-RPC connection. The session
// file remains the single source of truth; apps are materialized views.
type Server struct {
	conn *Conn

	mu        sync.Mutex
	sessions  map[string]*Session
	autoTitle bool
}

// New builds the server on the connection.
func New(conn *Conn, opts ...Option) *Server {
	s := &Server{conn: conn, sessions: map[string]*Session{}}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Option configures a Server at construction.
type Option func(*Server)

// AutoTitle enables background LLM session-title generation. The desktop's
// `scode serve` opts in; in-process tests omit it so their scripted providers
// see no extra request.
func AutoTitle() Option { return func(s *Server) { s.autoTitle = true } }

// Handler is the Conn request dispatcher entry point.
func (s *Server) Handler() func(ctx context.Context, method string, params json.RawMessage) (any, error) {
	return s.handle
}

// Shutdown disposes every live session (MCP servers, session files).
func (s *Server) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.sessions {
		h.App.Close() //nolint:errcheck
	}
	// The server is the process boundary: whatever is left in the
	// background-task registry dies with it (each App.Close already
	// stopped its own session's tasks).
	tools.KillAllBackgroundTasks()
}

// ---------------------------------------------------------------------------
// methods
// ---------------------------------------------------------------------------

func (s *Server) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": ProtocolVersion,
			"server":          "scode",
			"capabilities":    []string{"sessions", "sessionDelete", "sessionPrune", "sessionUsage", "sandbox", "approvals", "planMode", "compaction", "steer", "models", "modelConfig", "tasks"},
		}, nil
	case "session/create":
		return s.createSession(cli.Options{})
	case "session/resume":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" {
			return nil, fmt.Errorf("session/resume needs sessionId")
		}
		return s.createSession(cli.Options{Resume: p.SessionID})
	case "session/list":
		return s.listSessions()
	case "session/delete":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" {
			return nil, fmt.Errorf("session/delete needs sessionId")
		}
		return s.deleteSession(p.SessionID)
	case "session/prune":
		return s.pruneEmptySessions()
	case "session/fork":
		var p struct {
			SessionID string `json:"sessionId"`
			Upto      int    `json:"upto,omitempty"`
			// TailTurns anchors the cut at "the k-th user turn from the
			// end (reply included)" instead of a raw entry count — the
			// desktop's per-turn fork button counts user rows the same
			// way, and counting from the tail is immune to compaction
			// trimming the front of the projected view.
			TailTurns int `json:"tailTurns,omitempty"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" {
			return nil, fmt.Errorf("session/fork needs sessionId")
		}
		upto := p.Upto
		if p.TailTurns > 0 {
			var err error
			upto, err = forkUpToByTailTurns(p.SessionID, p.TailTurns)
			if err != nil {
				return nil, err
			}
		}
		return s.forkSession(p.SessionID, upto)
	case "session/prompt":
		var p struct {
			SessionID string   `json:"sessionId"`
			Text      string   `json:"text"`
			Images    []string `json:"images,omitempty"` // base64 payloads
		}
		if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" {
			return nil, fmt.Errorf("session/prompt needs sessionId and text")
		}
		return s.prompt(p.SessionID, p.Text, decodeImages(p.Images))
	case "session/steer":
		var p struct {
			SessionID string `json:"sessionId"`
			Text      string `json:"text"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		h, err := s.get(p.SessionID)
		if err != nil {
			return nil, err
		}
		h.App.Agent.Steer(p.Text)
		return map[string]any{"ok": true}, nil
	case "session/cancel":
		var p struct{ SessionID string }
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		h, err := s.get(p.SessionID)
		if err != nil {
			return nil, err
		}
		h.runMu.Lock()
		if h.cancel != nil {
			h.cancel()
		}
		h.runMu.Unlock()
		return map[string]any{"ok": true}, nil
	case "session/compact":
		var p struct {
			SessionID string `json:"sessionId"`
			Custom    string `json:"custom,omitempty"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		h, err := s.get(p.SessionID)
		if err != nil {
			return nil, err
		}
		before, after, err := h.App.CompactNowReport(ctx, p.Custom)
		switch {
		case err == nil:
			return map[string]any{"ok": true, "tokensBefore": before, "tokensAfter": after}, nil
		case errors.Is(err, cli.ErrNothingToCompact):
			// An expected state, not a failure: the history already
			// fits inside the kept tail.
			return map[string]any{"ok": false, "reason": "nothing to compact", "tokensBefore": before, "tokensAfter": after}, nil
		default:
			return nil, err
		}
	case "session/reload":
		// Hot reload: re-run skill discovery mid-session (the desktop's
		// /reload — prompts pass through verbatim, so this is an RPC).
		var p struct{ SessionID string }
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		h, err := s.get(p.SessionID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "summary": h.App.ReloadSkills()}, nil
	case "session/cost":
		var p struct{ SessionID string }
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		h, err := s.get(p.SessionID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"text": h.App.CostReport()}, nil
	case "session/usage":
		var p struct{ SessionID string }
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		h, err := s.get(p.SessionID)
		if err != nil {
			return nil, err
		}
		return h.App.UsageReport(), nil
	case "session/sandbox":
		var p struct {
			SessionID string `json:"sessionId"`
			Mode      string `json:"mode"` // empty = query; else read-only | workspace-write | danger-full-access
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		h, err := s.get(p.SessionID)
		if err != nil {
			return nil, err
		}
		if p.Mode != "" {
			if err := h.App.SetSandboxMode(p.Mode); err != nil {
				return nil, err
			}
		}
		return map[string]any{"sandbox": h.App.SandboxMode()}, nil
	case "session/mode":
		var p struct {
			SessionID string `json:"sessionId"`
			Mode      string `json:"mode"` // "plan" | "default"
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		h, err := s.get(p.SessionID)
		if err != nil {
			return nil, err
		}
		switch p.Mode {
		case "plan":
			h.App.SetPlanMode(true)
		case "default":
			h.App.SetPlanMode(false)
		default:
			return nil, fmt.Errorf("unknown mode %q", p.Mode)
		}
		return s.stateOf(p.SessionID, h)
	case "models/list":
		settings, err := config.LoadSettings()
		if err != nil {
			return nil, err
		}
		return modelsResult(settings), nil
	case "models/save":
		var p struct {
			Name     string `json:"name"`
			Original string `json:"original"`
			config.ProviderConfig
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := config.UpsertProvider(p.Name, p.ProviderConfig, p.Original); err != nil {
			return nil, err
		}
		s, err := config.LoadSettings()
		if err != nil {
			return nil, err
		}
		return modelsResult(s), nil
	case "models/delete":
		var p struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
			return nil, fmt.Errorf("models/delete needs name")
		}
		if err := config.DeleteProvider(p.Name); err != nil {
			return nil, err
		}
		s, err := config.LoadSettings()
		if err != nil {
			return nil, err
		}
		return modelsResult(s), nil
	case "models/default":
		var p struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.Provider == "" {
			return nil, fmt.Errorf("models/default needs provider")
		}
		if err := config.SetDefault(p.Provider, p.Model); err != nil {
			return nil, err
		}
		s, err := config.LoadSettings()
		if err != nil {
			return nil, err
		}
		return modelsResult(s), nil
	case "session/model":
		var p struct {
			SessionID string `json:"sessionId"`
			Provider  string `json:"provider"`
			Model     string `json:"model"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" {
			return nil, fmt.Errorf("session/model needs sessionId")
		}
		h, err := s.get(p.SessionID)
		if err != nil {
			return nil, err
		}
		if h.running.Load() {
			return nil, fmt.Errorf("session %q has a run in flight; stop or wait before switching models", p.SessionID)
		}
		if _, _, err := h.App.SetModel(p.Provider, p.Model); err != nil {
			return nil, err
		}
		return s.stateOf(p.SessionID, h)
	case "session/thinking":
		var p struct {
			SessionID string `json:"sessionId"`
			Level     string `json:"level"` // "" | off | low | medium | high
		}
		if err := json.Unmarshal(params, &p); err != nil || p.SessionID == "" {
			return nil, fmt.Errorf("session/thinking needs sessionId")
		}
		h, err := s.get(p.SessionID)
		if err != nil {
			return nil, err
		}
		if h.running.Load() {
			return nil, fmt.Errorf("session %q has a run in flight; stop or wait before switching reasoning effort", p.SessionID)
		}
		if err := h.App.SetThinkingLevel(p.Level); err != nil {
			return nil, err
		}
		return s.stateOf(p.SessionID, h)
	case "session/state":
		var p struct{ SessionID string }
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		h, err := s.get(p.SessionID)
		if err != nil {
			return nil, err
		}
		return s.stateOf(p.SessionID, h)
	case "session/transcript":
		var p struct{ SessionID string }
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		h, err := s.get(p.SessionID)
		if err != nil {
			return nil, err
		}
		// The projected conversation, provider-neutral messages as-is;
		// system messages (prompt, deltas) never cross the protocol.
		var msgs []llm.Message
		for _, m := range h.App.Tr.Messages() {
			if m.Role != llm.RoleSystem {
				msgs = append(msgs, m)
			}
		}
		return map[string]any{"messages": msgs}, nil
	case "session/plan":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		h, err := s.get(p.SessionID)
		if err != nil {
			return nil, err
		}
		// The plan checklist is per-session state (the update_plan
		// tracker, replayed from the log on resume); null when the
		// session has no plan yet.
		plan, ok := h.App.PlanState()
		if !ok {
			return map[string]any{"plan": nil}, nil
		}
		return map[string]any{"plan": plan}, nil
	case "session/tasks":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		// The background-task registry is process-wide; filtering by
		// session keeps one session's panel from listing another's
		// commands. An empty sessionId lists the whole server.
		return map[string]any{"tasks": tools.ListBackgroundTasks(p.SessionID)}, nil
	case "session/task-kill":
		var p struct {
			SessionID string `json:"sessionId"`
			TaskID    int    `json:"taskId"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := tools.KillBackgroundTask(p.TaskID); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	default:
		return nil, &methodNotFound{method}
	}
}

type methodNotFound struct{ method string }

func (e *methodNotFound) Error() string { return "method not found: " + e.method }

// stateOf reports the client-facing session view ({mode, pending} per
// the design doc; pending is always false in v1 — CLI-mode switches
// commit immediately).
func (s *Server) stateOf(id string, h *Session) (map[string]any, error) {
	mode := "default"
	if h.App.PlanMode() {
		mode = "plan"
	}
	provider, model := h.App.CurrentModel()
	return map[string]any{
		"sessionId": id,
		"mode":      mode,
		"pending":   false,
		"running":   h.running.Load(),
		"cost":      h.App.CostReport(),
		"provider":  provider,
		"model":     model,
		"thinking":  h.App.CurrentThinkingLevel(),
		"sandbox":   h.App.SandboxMode(),
	}, nil
}

// ---------------------------------------------------------------------------
// session lifecycle
// ---------------------------------------------------------------------------

func (s *Server) createSession(opts cli.Options) (any, error) {
	// Resuming an already-live session is idempotent: hand back the open
	// handle's state. Closing it to re-open would race the in-flight
	// run's persistence ("file already closed") and orphan the turn —
	// this is how a re-attaching client (renderer reload, second
	// window) rejoins a running session.
	if opts.Resume != "" {
		s.mu.Lock()
		live := s.sessions[opts.Resume]
		s.mu.Unlock()
		if live != nil {
			return s.stateOf(opts.Resume, live)
		}
	}
	opts.GenerateTitle = s.autoTitle
	// The session id exists only after Setup; the approver stamps it
	// via this holder (approvals only fire during runs, long after).
	var idHolder atomic.Value
	opts.Approver = NewApprover(s.conn, func() string {
		v, _ := idHolder.Load().(string)
		return v
	})
	app, err := cli.Setup(opts)
	if err != nil {
		return nil, err
	}
	id := app.Sess.Header().ID
	idHolder.Store(id)
	h := &Session{App: app}
	s.mu.Lock()
	if old := s.sessions[id]; old != nil {
		old.App.Close() //nolint:errcheck // session-exclusive: resume supersedes the live handle
	}
	s.sessions[id] = h
	s.mu.Unlock()
	provider, model := app.CurrentModel()
	return map[string]any{"sessionId": id, "mode": modeOf(app), "pending": false, "provider": provider, "model": model, "thinking": app.CurrentThinkingLevel(), "sandbox": app.SandboxMode()}, nil
}

func (s *Server) listSessions() (any, error) {
	// The store root derives from the process cwd like the CLI; probe
	// with a throwaway app-free listing via a temporary store handle.
	ids, err := listSessionIDs()
	if err != nil {
		return nil, err
	}
	return map[string]any{"sessions": ids}, nil
}

// deleteSession closes the live handle (if any) and removes the
// session file. Deleting a running session first cancels its run.
func (s *Server) deleteSession(id string) (any, error) {
	s.mu.Lock()
	if h, ok := s.sessions[id]; ok {
		h.runMu.Lock()
		if h.cancel != nil {
			h.cancel()
		}
		h.runMu.Unlock()
		h.App.Close() //nolint:errcheck
		delete(s.sessions, id)
	}
	s.mu.Unlock()

	// A deleted session must not leave its detached commands behind.
	tools.KillBackgroundTasksForSession(id)

	store, err := sessionStore()
	if err != nil {
		return nil, err
	}
	if err := store.Delete(id); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// pruneEmptySessions removes sessions that never received a message
// (header/model/mode lines only). Live sessions are skipped: their
// files are open and a still-drafting session is not clutter yet.
func (s *Server) pruneEmptySessions() (any, error) {
	store, err := sessionStore()
	if err != nil {
		return nil, err
	}
	ids, err := store.List()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	live := make(map[string]bool, len(s.sessions))
	for id := range s.sessions {
		live[id] = true
	}
	s.mu.Unlock()

	deleted := 0
	for _, id := range ids {
		if live[id] {
			continue
		}
		has, err := store.HasMessages(id)
		if err != nil || has {
			continue
		}
		if store.Delete(id) == nil {
			deleted++
		}
	}
	return map[string]any{"deleted": deleted}, nil
}

func (s *Server) forkSession(id string, upto int) (any, error) {
	newID, err := forkSession(id, upto)
	if err != nil {
		return nil, err
	}
	return map[string]any{"sessionId": newID}, nil
}

func (s *Server) get(id string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.sessions[id]
	if !ok {
		return nil, fmt.Errorf("unknown session %q (create or resume it first)", id)
	}
	return h, nil
}

func modeOf(app *cli.App) string {
	if app.PlanMode() {
		return "plan"
	}
	return "default"
}

// modelsResult is the shared shape of the model RPCs: the switchable
// choices, the full profiles the settings editor needs, and the current
// default.
func modelsResult(s *config.Settings) map[string]any {
	return map[string]any{
		"models":   config.ListModels(s),
		"profiles": config.ListProfiles(s),
		"default":  map[string]any{"provider": s.DefaultProvider, "model": s.DefaultModel},
		// the settings-level reasoning effort; sessions without their own
		// thinking entry start here ("" = provider default)
		"thinking": s.DefaultThinkingLevel,
	}
}

// ---------------------------------------------------------------------------
// runs + event streaming
// ---------------------------------------------------------------------------

// maxPromptImages caps explicit attachments per prompt (provider image
// limits and cost); each payload is content-sniffed like the CLI's
// clipboard images (cli.ImageBlock), so a bogus payload is skipped rather
// than bricking every later provider request.
const maxPromptImages = 8

// decodeImages turns the client's base64 payloads into image blocks,
// tolerating a data URL prefix ("data:image/png;base64,…") and skipping
// payloads that are not decodable, supported images.
func decodeImages(encoded []string) []llm.Block {
	var out []llm.Block
	for _, s := range encoded {
		if len(out) >= maxPromptImages {
			break
		}
		if strings.HasPrefix(s, "data:") {
			if i := strings.IndexByte(s, ','); i >= 0 {
				s = s[i+1:]
			}
		}
		data, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			continue
		}
		if block, ok := cli.ImageBlock(data); ok {
			out = append(out, block)
		}
	}
	return out
}

// prompt starts a run; events stream back as session/event
// notifications. Returns once the run has started.
func (s *Server) prompt(id, text string, images []llm.Block) (any, error) {
	h, err := s.get(id)
	if err != nil {
		return nil, err
	}
	if !h.running.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("session %q already has a run in flight", id)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.runMu.Lock()
	h.cancel = cancel
	h.runMu.Unlock()
	go func() {
		defer h.running.Store(false)
		out := make(chan agent.Event, 256)
		done := make(chan error, 1)
		go func() { done <- h.App.Run(ctx, out, text, images...) }()
		for ev := range out {
			s.conn.Notify("session/event", mapEvent(id, ev)) //nolint:errcheck
		}
		if err := <-done; err != nil {
			s.conn.Notify("session/event", map[string]any{ //nolint:errcheck
				"sessionId": id, "type": "run_error", "error": err.Error(),
			})
		}
	}()
	return map[string]any{"ok": true}, nil
}

// mapEvent converts an agent event to its wire shape. Event schema
// stays provider-neutral (llm.Message passes through as-is; system
// prompt and tool declarations never cross the protocol).
func mapEvent(sessionID string, ev agent.Event) map[string]any {
	m := map[string]any{"sessionId": sessionID, "type": string(ev.Type)}
	switch {
	case ev.LLM != nil:
		lm := map[string]any{"type": string(ev.LLM.Type)}
		if ev.LLM.Delta != "" {
			lm["delta"] = ev.LLM.Delta
		}
		if ev.LLM.Reason != "" {
			lm["reason"] = string(ev.LLM.Reason)
		}
		m["llm"] = lm
	case ev.Call != nil:
		call := map[string]any{"name": ev.Call.Name, "id": ev.Call.ID}
		if len(ev.Call.Arguments) > 0 {
			call["arguments"] = json.RawMessage(ev.Call.Arguments)
		}
		m["call"] = call
		if ev.Result != nil {
			var text string
			for _, b := range ev.Result.Content {
				if b.Kind == llm.BlockText {
					text += b.Text
				}
			}
			m["result"] = map[string]any{"text": text, "isError": ev.Result.IsError}
		}
	case ev.Message != nil:
		m["message"] = ev.Message
	}
	if ev.Err != nil {
		m["error"] = ev.Err.Error()
	}
	return m
}

// ---------------------------------------------------------------------------
// approvals (server→client reverse requests)
// ---------------------------------------------------------------------------

// rpcApprover implements cli.AskReviewer over the connection: each ask
// becomes an approval/request the client answers (ACP's
// session/request_permission shape).
type rpcApprover struct {
	conn      *Conn
	sessionOf func() string // owning session, stamped on each request
}

// NewApprover builds the approval bridge for one session.
func NewApprover(conn *Conn, sessionOf func() string) *rpcApprover {
	return &rpcApprover{conn: conn, sessionOf: sessionOf}
}

type approvalResponse struct {
	Decision string `json:"decision"`           // allow | deny | allow_session | allow_project | approve | revise
	Feedback string `json:"feedback,omitempty"` // deny reason / revision guidance
}

func (r *rpcApprover) Ask(ctx context.Context, call llm.Block, rule *permission.Rule, exact string) cli.ApprovalResult {
	kind, value := permission.CallSummary(call)
	var resp approvalResponse
	err := r.conn.Call(ctx, "approval/request", map[string]any{
		"sessionId": r.sessionOf(),
		"kind":      "tool",
		"tool":      call.Name,
		"argKind":   kind,
		"argValue":  value,
		"rule":      rule.Raw,
		"exact":     exact,
	}, &resp)
	if err != nil {
		return cli.ApprovalResult{Reason: "approval request failed: " + err.Error()}
	}
	switch resp.Decision {
	case "allow":
		return cli.ApprovalResult{Allow: true}
	case "allow_session":
		return cli.ApprovalResult{Allow: true, SessionRule: exact}
	case "allow_project":
		return cli.ApprovalResult{Allow: true, ProjectRule: exact}
	default:
		reason := resp.Feedback
		if reason == "" {
			reason = "denied by the client"
		}
		return cli.ApprovalResult{Reason: reason}
	}
}

// ReviewPlan implements planmode.Reviewer over RPC.
func (r *rpcApprover) ReviewPlan(tc agent.ToolContext, plan string) (bool, string) {
	var resp approvalResponse
	err := r.conn.Call(tc.Ctx, "approval/request", map[string]any{
		"sessionId": r.sessionOf(),
		"kind":      "plan",
		"plan":      plan,
	}, &resp)
	if err != nil {
		return false, "the plan review could not reach the client: " + err.Error()
	}
	if resp.Decision == "approve" {
		return true, ""
	}
	return false, resp.Feedback
}

// ReviewSandboxEscalation asks the client to widen one call's sandbox
// mode (kind "sandbox"). allow = this call only; allow_session /
// allow_project = also switch the session's standing mode.
func (r *rpcApprover) ReviewSandboxEscalation(ctx context.Context, req agent.EscalationRequest) agent.EscalationResult {
	var resp approvalResponse
	err := r.conn.Call(ctx, "approval/request", map[string]any{
		"sessionId":     r.sessionOf(),
		"kind":          "sandbox",
		"tool":          req.Tool,
		"detail":        req.Detail,
		"currentMode":   req.CurrentMode,
		"requestedMode": req.RequestedMode,
		"justification": req.Justification,
	}, &resp)
	if err != nil {
		return agent.EscalationResult{Reason: "approval request failed: " + err.Error()}
	}
	switch resp.Decision {
	case "allow":
		return agent.EscalationResult{Approved: true}
	case "allow_session", "allow_project":
		return agent.EscalationResult{Approved: true, ApplyToSession: true}
	default:
		reason := resp.Feedback
		if reason == "" {
			reason = "denied by the client"
		}
		return agent.EscalationResult{Reason: reason}
	}
}

// Run serves the JSON-RPC session protocol over r/w until EOF or ctx
// cancellation (production: stdio; tests: pipes). stdout discipline:
// callers must route ALL logging to stderr.
func Run(ctx context.Context, r io.Reader, w io.Writer, opts ...Option) error {
	var srv *Server
	conn := NewConn(w, func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		return srv.handle(ctx, method, params)
	}, nil)
	srv = New(conn, opts...)
	defer srv.Shutdown()
	return conn.Serve(ctx, r)
}

// sessionStore resolves the session store for the process cwd (the
// server inherits the CLI's layout: <cfgDir>/sessions/<safe-cwd>).
func sessionStore() (*session.Store, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	cfgDir, err := config.EnsureDir()
	if err != nil {
		return nil, err
	}
	return session.NewStore(session.DefaultRoot(cfgDir, cwd))
}

func listSessionIDs() ([]string, error) {
	store, err := sessionStore()
	if err != nil {
		return nil, err
	}
	return store.List()
}

func forkSession(id string, upto int) (string, error) {
	store, err := sessionStore()
	if err != nil {
		return "", err
	}
	return store.Fork(id, upto)
}

// forkUpToByTailTurns maps "the k-th user turn from the end, its reply
// included" to a path-entry cut index for Store.Fork: walk backwards
// counting user-role message entries to the anchor, then cut just
// before the NEXT user message so the whole turn survives. Fewer turns
// on disk than asked (the view ran ahead of persistence) forks
// everything.
func forkUpToByTailTurns(id string, tailTurns int) (int, error) {
	store, err := sessionStore()
	if err != nil {
		return 0, err
	}
	rec, err := store.Load(id)
	if err != nil {
		return 0, err
	}
	path := rec.Path()
	anchor := -1
	seen := 0
	for i := len(path) - 1; i >= 0 && anchor < 0; i-- {
		if e := path[i]; e.Msg != nil && e.Msg.Role == llm.RoleUser {
			seen++
			if seen == tailTurns {
				anchor = i
			}
		}
	}
	if anchor < 0 {
		return 0, nil
	}
	upto := len(path)
	for i := anchor + 1; i < len(path); i++ {
		if e := path[i]; e.Msg != nil && e.Msg.Role == llm.RoleUser {
			upto = i
			break
		}
	}
	return upto, nil
}
