package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"scode/internal/llm"
	"scode/internal/session"
)

// chatSSE writes one chat.completions streaming text response.
func chatSSE(w http.ResponseWriter, content string) {
	w.Header().Set("content-type", "text/event-stream")
	cb, _ := json.Marshal(content)
	for _, c := range []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%s}}]}`, cb),
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		`[DONE]`,
	} {
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
}

// chatSSEToolCall writes one streaming response carrying a tool call.
func chatSSEToolCall(w http.ResponseWriter, name, args string) {
	w.Header().Set("content-type", "text/event-stream")
	nb, _ := json.Marshal(name)
	ab, _ := json.Marshal(args)
	for _, c := range []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":%s,"arguments":%s}}]}}]}`, nb, ab),
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		`[DONE]`,
	} {
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
}

// testEnv points SCODE_DIR at a temp config dir with the fake provider,
// in a temp working directory (mirrors cli's e2e setup).
func testEnv(t *testing.T, srv *httptest.Server, perms map[string]any) {
	t.Helper()
	cfgDir := t.TempDir()
	proj := t.TempDir()
	settings := map[string]any{
		"defaultProvider":  "openai-compat",
		"compactionTokens": -1,
		"providers": map[string]any{
			"openai-compat": map[string]any{"apiKey": "test", "baseUrl": srv.URL, "model": "test-model"},
		},
	}
	if perms != nil {
		settings["permissions"] = perms
	}
	sb, _ := json.Marshal(settings)
	os.WriteFile(filepath.Join(cfgDir, "settings.json"), sb, 0o644) //nolint:errcheck
	t.Setenv("SCODE_DIR", cfgDir)
	t.Chdir(proj)
}

// testClient is the pipe-driven client end: collects notifications,
// answers reverse requests with the scripted approval decision.
type testClient struct {
	conn     *Conn
	mu       sync.Mutex
	events   []map[string]any
	approval *approvalResponse
	gotReq   []map[string]any
}

func newTestClient(t *testing.T, srv2cliR io.Reader, cli2srvW io.Writer, approval *approvalResponse) *testClient {
	t.Helper()
	c := &testClient{approval: approval}
	c.conn = NewConn(cli2srvW, func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		c.mu.Lock()
		c.gotReq = append(c.gotReq, map[string]any{"method": method, "params": p})
		c.mu.Unlock()
		if method == "approval/request" && c.approval != nil {
			return c.approval, nil
		}
		return nil, fmt.Errorf("unexpected reverse call %s", method)
	}, func(method string, params json.RawMessage) {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		p["method"] = method
		c.mu.Lock()
		c.events = append(c.events, p)
		c.mu.Unlock()
	})
	go c.conn.Serve(context.Background(), srv2cliR) //nolint:errcheck
	return c
}

func (c *testClient) waitEvent(t *testing.T, typ string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for _, e := range c.events {
			if e["type"] == typ {
				c.mu.Unlock()
				return e
			}
		}
		c.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("event %q never arrived; got %d events", typ, len(c.events))
	return nil
}

func callRPC(t *testing.T, c *testClient, method string, params any) map[string]any {
	t.Helper()
	var out map[string]any
	if err := c.conn.Call(context.Background(), method, params, &out); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return out
}

// startPair wires a server and a client over pipe pairs; cleanup shuts
// the server down (Windows holds session files open otherwise).
func startPair(t *testing.T, approval *approvalResponse) *testClient {
	t.Helper()
	srv2cliR, srv2cliW := io.Pipe()
	cli2srvR, cli2srvW := io.Pipe()
	var srv *Server
	conn := NewConn(srv2cliW, func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		return srv.handle(ctx, method, params)
	}, nil)
	srv = New(conn)
	go conn.Serve(context.Background(), cli2srvR) //nolint:errcheck
	t.Cleanup(srv.Shutdown)
	return newTestClient(t, srv2cliR, cli2srvW, approval)
}

// Full round trip: initialize → create → prompt → streamed events →
// state. The session file is the durable truth underneath.
func TestServerPromptE2E(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatSSE(w, "hello from the model")
	}))
	defer srv.Close()
	testEnv(t, srv, nil)

	client := startPair(t, nil)

	init := callRPC(t, client, "initialize", nil)
	if init["protocolVersion"].(float64) != ProtocolVersion {
		t.Fatalf("protocolVersion = %v", init["protocolVersion"])
	}
	created := callRPC(t, client, "session/create", nil)
	id := created["sessionId"].(string)
	if id == "" || created["mode"] != "default" {
		t.Fatalf("create = %v", created)
	}

	callRPC(t, client, "session/prompt", map[string]any{"sessionId": id, "text": "hi"})
	end := client.waitEvent(t, "agent_end")
	if end["sessionId"] != id {
		t.Fatalf("agent_end for %v", end["sessionId"])
	}

	// Text streamed as llm deltas before agent_end.
	client.mu.Lock()
	var sawDelta bool
	for _, e := range client.events {
		if e["type"] == "llm" {
			if lm, ok := e["llm"].(map[string]any); ok && strings.Contains(fmt.Sprint(lm["delta"]), "hello") {
				sawDelta = true
			}
		}
	}
	client.mu.Unlock()
	if !sawDelta {
		t.Fatal("no text delta in event stream")
	}

	st := callRPC(t, client, "session/state", map[string]any{"sessionId": id})
	if st["running"].(bool) {
		t.Fatal("session still marked running after agent_end")
	}

	// The session is listable and resumable afterwards.
	list := callRPC(t, client, "session/list", nil)
	if !strings.Contains(fmt.Sprint(list["sessions"]), id) {
		t.Fatalf("session %s missing from list %v", id, list["sessions"])
	}
	resumed := callRPC(t, client, "session/resume", map[string]any{"sessionId": id})
	if resumed["sessionId"] != id {
		t.Fatalf("resume = %v", resumed)
	}
}

// Prompt image attachments: base64 payloads ride session/prompt and reach
// the provider as an image_url part; bogus payloads are skipped (content
// sniffing, like the CLI's clipboard images).
func TestServerPromptWithImages(t *testing.T) {
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body.Store(string(b))
		chatSSE(w, "got the image")
	}))
	defer srv.Close()
	testEnv(t, srv, nil)

	client := startPair(t, nil)
	created := callRPC(t, client, "session/create", nil)
	id := created["sessionId"].(string)

	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString(pngBuf.Bytes())
	callRPC(t, client, "session/prompt", map[string]any{
		"sessionId": id,
		"text":      "what is this?",
		// data URL prefix tolerated; the bogus payload must be dropped.
		"images": []string{"data:image/png;base64," + b64, "not-an-image"},
	})
	client.waitEvent(t, "agent_end")

	got, _ := body.Load().(string)
	if !strings.Contains(got, `"image_url"`) {
		t.Fatalf("request carries no image_url part:\n%s", got)
	}
	if !strings.Contains(got, b64) {
		t.Fatalf("request image payload mismatch:\n%s", got)
	}
	if strings.Count(got, `"type":"image_url"`) != 1 {
		t.Fatalf("bogus payload was not skipped:\n%s", got)
	}
}

// Ask rules route through the reverse approval channel: the tool runs
// only after the client allows.
func TestServerApprovalE2E(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			chatSSEToolCall(w, "bash", `{"command":"echo approved-output"}`)
			return
		}
		chatSSE(w, "done")
	}))
	defer srv.Close()
	testEnv(t, srv, map[string]any{"ask": []string{"bash(echo:*)"}})

	client := startPair(t, &approvalResponse{Decision: "allow"})

	created := callRPC(t, client, "session/create", nil)
	id := created["sessionId"].(string)
	callRPC(t, client, "session/prompt", map[string]any{"sessionId": id, "text": "say something"})

	end := client.waitEvent(t, "tool_end")
	res, _ := end["result"].(map[string]any)
	if res["isError"].(bool) {
		t.Fatalf("approved tool should have run: %v", res)
	}
	if !strings.Contains(fmt.Sprint(res["text"]), "approved-output") {
		t.Fatalf("tool result = %v", res["text"])
	}
	client.mu.Lock()
	var sawApproval bool
	for _, r := range client.gotReq {
		if r["method"] == "approval/request" {
			sawApproval = true
			if p, ok := r["params"].(map[string]any); !ok || p["kind"] != "tool" || p["rule"] != "bash(echo:*)" {
				t.Fatalf("approval params = %v", r["params"])
			}
		}
	}
	client.mu.Unlock()
	if !sawApproval {
		t.Fatal("client never received approval/request")
	}
}

// Transcript replay: after a run, session/transcript returns the
// projected conversation (user/assistant/tool messages, no system).
func TestServerTranscriptE2E(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			chatSSEToolCall(w, "bash", `{"command":"echo replay-me"}`)
			return
		}
		chatSSE(w, "all done")
	}))
	defer srv.Close()
	testEnv(t, srv, nil)

	client := startPair(t, nil)
	created := callRPC(t, client, "session/create", nil)
	id := created["sessionId"].(string)
	callRPC(t, client, "session/prompt", map[string]any{"sessionId": id, "text": "run it"})
	client.waitEvent(t, "agent_end")

	r := callRPC(t, client, "session/transcript", map[string]any{"sessionId": id})
	msgs, _ := r["messages"].([]any)
	if len(msgs) < 4 {
		t.Fatalf("transcript = %d messages, want user+assistant+tool+assistant", len(msgs))
	}
	var sawCall, sawResult bool
	for _, m := range msgs {
		mm := m.(map[string]any)
		for _, b := range mm["content"].([]any) {
			bb := b.(map[string]any)
			switch bb["kind"] {
			case "toolCall":
				if bb["name"] == "bash" && bb["id"] != "" {
					sawCall = true
				}
			case "toolResult":
				if nested, _ := bb["content"].([]any); len(nested) > 0 {
					if strings.Contains(fmt.Sprint(nested[0].(map[string]any)["text"]), "replay-me") {
						sawResult = true
					}
				}
			}
		}
	}
	if !sawCall || !sawResult {
		t.Fatalf("transcript missing tool call (%v) or result (%v)", sawCall, sawResult)
	}
	// Live events carried the call id too (card pairing on the client).
	client.mu.Lock()
	var sawID bool
	for _, e := range client.events {
		if e["type"] == "tool_start" {
			if c, _ := e["call"].(map[string]any); c != nil && c["id"] != "" {
				sawID = true
			}
		}
	}
	client.mu.Unlock()
	if !sawID {
		t.Fatal("tool_start event without call id")
	}
}

// Compacting a fresh session is an expected state, not an error.
func TestServerCompactNothingE2E(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	testEnv(t, srv, nil)

	client := startPair(t, nil)
	created := callRPC(t, client, "session/create", nil)
	id := created["sessionId"].(string)

	r := callRPC(t, client, "session/compact", map[string]any{"sessionId": id})
	if r["ok"] != false || r["reason"] != "nothing to compact" {
		t.Fatalf("compact on fresh session = %v, want graceful ok:false", r)
	}
}

// Model listing and switching: a switch updates state, persists as an
// append-only entry, and replays on resume.
func TestServerModelSwitchE2E(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatSSE(w, "ok")
	}))
	defer srv.Close()
	testEnv(t, srv, nil)

	client := startPair(t, nil)
	created := callRPC(t, client, "session/create", nil)
	id := created["sessionId"].(string)
	if created["provider"] != "openai-compat" || created["model"] != "test-model" {
		t.Fatalf("initial model = %v/%v", created["provider"], created["model"])
	}

	models := callRPC(t, client, "models/list", nil)
	list, _ := models["models"].([]any)
	if len(list) == 0 {
		t.Fatalf("models/list = %v", models)
	}

	// A bare model id keeps the current provider.
	st := callRPC(t, client, "session/model", map[string]any{"sessionId": id, "provider": "openai-compat", "model": "test-model-2"})
	if st["model"] != "test-model-2" || st["provider"] != "openai-compat" {
		t.Fatalf("after switch = %v", st)
	}

	// An unresolvable model leaves the session on its previous model.
	var out map[string]any
	if err := client.conn.Call(context.Background(), "session/model", map[string]any{"sessionId": id, "provider": "nope", "model": "x"}, &out); err == nil {
		t.Fatal("unknown provider should error")
	}
	state := callRPC(t, client, "session/state", map[string]any{"sessionId": id})
	if state["model"] != "test-model-2" {
		t.Fatalf("failed switch changed model: %v", state["model"])
	}

	// Persisted: resume replays the switch.
	resumed := callRPC(t, client, "session/resume", map[string]any{"sessionId": id})
	if resumed["model"] != "test-model-2" {
		t.Fatalf("resume model = %v, want test-model-2", resumed["model"])
	}
}

// Errors surface as JSON-RPC errors, never panics.
func TestServerErrorPaths(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	testEnv(t, srv, nil)

	client := startPair(t, nil)

	var out map[string]any
	if err := client.conn.Call(context.Background(), "bogus/method", nil, &out); err == nil {
		t.Fatal("unknown method should error")
	}
	if err := client.conn.Call(context.Background(), "session/prompt", map[string]any{"sessionId": "nope", "text": "x"}, &out); err == nil {
		t.Fatal("unknown session should error")
	}
}

// Model profile CRUD through the server: create a custom profile,
// switch a live session onto it, then delete it.
func TestServerModelConfigE2E(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatSSE(w, "ok")
	}))
	defer srv.Close()
	testEnv(t, srv, nil)

	client := startPair(t, nil)
	created := callRPC(t, client, "session/create", nil)
	id := created["sessionId"].(string)

	// Create a custom profile pointing at the fake OpenAI-compatible endpoint.
	r := callRPC(t, client, "models/save", map[string]any{
		"name": "kimi", "protocol": "openai-compat",
		"baseUrl": srv.URL, "apiKey": "sk-k", "model": "k3",
	})
	if !profilesContain(t, r, "kimi", "k3") {
		t.Fatalf("saved profile missing: %v", r["profiles"])
	}

	// Mark it as the default: a fresh session resolves to it.
	r = callRPC(t, client, "models/default", map[string]any{"provider": "kimi", "model": "k3"})
	def, _ := r["default"].(map[string]any)
	if def["provider"] != "kimi" || def["model"] != "k3" {
		t.Fatalf("default = %v", def)
	}
	created2 := callRPC(t, client, "session/create", nil)
	if created2["provider"] != "kimi" || created2["model"] != "k3" {
		t.Fatalf("new session should use the default: %v", created2)
	}

	// Switch the live session onto it.
	st := callRPC(t, client, "session/model", map[string]any{"sessionId": id, "provider": "kimi", "model": "k3"})
	if st["provider"] != "kimi" || st["model"] != "k3" {
		t.Fatalf("switch = %v", st)
	}

	// Delete removes it from the list.
	r = callRPC(t, client, "models/delete", map[string]any{"name": "kimi"})
	if profilesContain(t, r, "kimi", "") {
		t.Fatalf("profile still listed after delete: %v", r["profiles"])
	}
}

func profilesContain(t *testing.T, r map[string]any, name, model string) bool {
	t.Helper()
	list, _ := r["profiles"].([]any)
	for _, p := range list {
		pp, _ := p.(map[string]any)
		if pp["name"] == name && (model == "" || pp["model"] == model) {
			return true
		}
	}
	return false
}

// session/delete removes the file and closes the live handle;
// session/prune sweeps on-disk sessions that never received a message.
func TestServerDeleteAndPruneE2E(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatSSE(w, "hi")
	}))
	defer srv.Close()
	testEnv(t, srv, nil)

	client := startPair(t, nil)

	// A live, non-empty session: deleted via RPC while resident.
	created := callRPC(t, client, "session/create", nil)
	id := created["sessionId"].(string)
	callRPC(t, client, "session/prompt", map[string]any{"sessionId": id, "text": "hi"})
	client.waitEvent(t, "agent_end")

	callRPC(t, client, "session/delete", map[string]any{"sessionId": id})
	list := callRPC(t, client, "session/list", nil)
	if strings.Contains(fmt.Sprint(list["sessions"]), id) {
		t.Fatalf("session %s still listed after delete", id)
	}
	// Deleted sessions are forgotten: resume must fail.
	var out map[string]any
	if err := client.conn.Call(context.Background(), "session/resume", map[string]any{"sessionId": id}, &out); err == nil {
		t.Fatal("resume of deleted session should error")
	}
	// Idempotent delete.
	callRPC(t, client, "session/delete", map[string]any{"sessionId": id})

	// Prune: an empty on-disk session goes, a non-empty one stays.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	st, err := session.NewStore(session.DefaultRoot(os.Getenv("SCODE_DIR"), cwd))
	if err != nil {
		t.Fatal(err)
	}
	emptySess, err := st.Create("", cwd)
	if err != nil {
		t.Fatal(err)
	}
	emptyID := emptySess.Header().ID
	if err := emptySess.Close(); err != nil {
		t.Fatal(err)
	}
	fullSess, err := st.Create("", cwd)
	if err != nil {
		t.Fatal(err)
	}
	fullID := fullSess.Header().ID
	if _, err := fullSess.Append(llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("hi")}}); err != nil {
		t.Fatal(err)
	}
	if err := fullSess.Close(); err != nil {
		t.Fatal(err)
	}

	res := callRPC(t, client, "session/prune", nil)
	if res["deleted"].(float64) != 1 {
		t.Fatalf("prune deleted = %v, want 1", res["deleted"])
	}
	list = callRPC(t, client, "session/list", nil)
	listed := fmt.Sprint(list["sessions"])
	if strings.Contains(listed, emptyID) {
		t.Fatalf("empty session %s survived prune", emptyID)
	}
	if !strings.Contains(listed, fullID) {
		t.Fatalf("non-empty session %s pruned", fullID)
	}
}

// session/usage reports structured occupancy + accumulated usage: after
// one prompted round the context estimate and input totals are non-zero.
func TestServerUsageE2E(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatSSE(w, "hello")
	}))
	defer srv.Close()
	testEnv(t, srv, nil)

	client := startPair(t, nil)
	created := callRPC(t, client, "session/create", nil)
	id := created["sessionId"].(string)

	// Before any prompt: zero usage, but the system prompt occupies context.
	pre := callRPC(t, client, "session/usage", map[string]any{"sessionId": id})
	if pre["contextTokens"].(float64) <= 0 {
		t.Fatalf("system prompt should occupy context: %v", pre)
	}
	if pre["input"].(float64) != 0 {
		t.Fatalf("no spend yet: %v", pre)
	}

	callRPC(t, client, "session/prompt", map[string]any{"sessionId": id, "text": "hi"})
	client.waitEvent(t, "agent_end")

	post := callRPC(t, client, "session/usage", map[string]any{"sessionId": id})
	if post["input"].(float64) != 10 || post["output"].(float64) != 5 {
		t.Fatalf("usage = %v, want input=10 output=5", post)
	}
	if _, ok := post["contextWindow"]; !ok {
		t.Fatalf("contextWindow missing: %v", post)
	}
}

// session/sandbox: default is danger-full-access, switching persists and
// folds back on resume; an invalid mode errors.
func TestServerSandboxE2E(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatSSE(w, "hi")
	}))
	defer srv.Close()
	testEnv(t, srv, nil)

	client := startPair(t, nil)
	created := callRPC(t, client, "session/create", nil)
	id := created["sessionId"].(string)
	if created["sandbox"] != "workspace-write" {
		t.Fatalf("default sandbox = %v", created["sandbox"])
	}

	st := callRPC(t, client, "session/sandbox", map[string]any{"sessionId": id, "mode": "read-only"})
	if st["sandbox"] != "read-only" {
		t.Fatalf("set = %v", st)
	}

	// Fold on resume: the override survives restart.
	resumed := callRPC(t, client, "session/resume", map[string]any{"sessionId": id})
	if resumed["sandbox"] != "read-only" {
		t.Fatalf("resume sandbox = %v", resumed["sandbox"])
	}

	// Query without mode returns the current value; invalid mode errors.
	q := callRPC(t, client, "session/sandbox", map[string]any{"sessionId": id})
	if q["sandbox"] != "read-only" {
		t.Fatalf("query = %v", q)
	}
	var out map[string]any
	if err := client.conn.Call(context.Background(), "session/sandbox", map[string]any{"sessionId": id, "mode": "yolo"}, &out); err == nil {
		t.Fatal("invalid mode should error")
	}
}

// Sandbox escalation over RPC: the model retries a confined write with
// sandbox_permissions + justification; the client sees a kind "sandbox"
// approval carrying the mode pair; allow approves the widened call.
func TestServerSandboxEscalationE2E(t *testing.T) {
	outside := filepath.Join(os.TempDir(), "..", fmt.Sprintf("scode-esc-e2e-%d.txt", time.Now().UnixNano()))
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			chatSSEToolCall(w, "write", fmt.Sprintf(`{"path":%q,"content":"x","sandbox_permissions":"danger-full-access","justification":"需要写共享目录"}`, outside))
			return
		}
		chatSSE(w, "done")
	}))
	defer srv.Close()
	testEnv(t, srv, nil)

	client := startPair(t, &approvalResponse{Decision: "allow"})
	created := callRPC(t, client, "session/create", nil)
	id := created["sessionId"].(string)
	callRPC(t, client, "session/sandbox", map[string]any{"sessionId": id, "mode": "workspace-write"})
	callRPC(t, client, "session/prompt", map[string]any{"sessionId": id, "text": "write the file"})
	defer os.Remove(outside) //nolint:errcheck

	end := client.waitEvent(t, "tool_end")
	res, _ := end["result"].(map[string]any)
	if res["isError"].(bool) {
		t.Fatalf("escalated write should have run: %v", res)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("escalated write did not land outside the workspace")
	}

	client.mu.Lock()
	var saw bool
	for _, r := range client.gotReq {
		if r["method"] != "approval/request" {
			continue
		}
		if p, ok := r["params"].(map[string]any); ok && p["kind"] == "sandbox" {
			saw = true
			if p["currentMode"] != "workspace-write" || p["requestedMode"] != "danger-full-access" {
				t.Fatalf("escalation params = %v", p)
			}
			if p["justification"] != "需要写共享目录" {
				t.Fatalf("justification = %v", p["justification"])
			}
		}
	}
	client.mu.Unlock()
	if !saw {
		t.Fatal("client never received the sandbox approval/request")
	}
}

// Background tasks cross the wire: a run_in_background bash call detaches,
// session/tasks lists it for the owning session, and session/task-kill stops
// it — the path the desktop task panel drives.
func TestServerBackgroundTasksE2E(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			chatSSEToolCall(w, "bash", `{"command":"sleep 30","run_in_background":true}`)
			return
		}
		chatSSE(w, "ok")
	}))
	defer srv.Close()
	testEnv(t, srv, nil)

	client := startPair(t, nil)
	created := callRPC(t, client, "session/create", nil)
	id := created["sessionId"].(string)
	// Unfenced so the command is a real shell (Git Bash / sh) on every
	// platform: the confined Windows shell is cmd.exe, which has no sleep.
	callRPC(t, client, "session/sandbox", map[string]any{"sessionId": id, "mode": "danger-full-access"})
	callRPC(t, client, "session/prompt", map[string]any{"sessionId": id, "text": "start a background job"})

	end := client.waitEvent(t, "tool_end")
	res, _ := end["result"].(map[string]any)
	text := fmt.Sprint(res["text"])
	if res["isError"].(bool) {
		t.Skipf("bash unavailable in this environment: %s", text)
	}
	if !strings.Contains(text, "background task #") {
		t.Fatalf("run_in_background result = %q", text)
	}

	listed := callRPC(t, client, "session/tasks", map[string]any{"sessionId": id})
	tasks, _ := listed["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("session/tasks = %v, want exactly one task", listed)
	}
	task, _ := tasks[0].(map[string]any)
	if task["state"] != "running" {
		t.Fatalf("fresh task state = %v", task["state"])
	}
	taskID := int(task["id"].(float64))

	// Another session's view must not include it.
	other := callRPC(t, client, "session/tasks", map[string]any{"sessionId": "some-other-session"})
	if otherTasks, _ := other["tasks"].([]any); len(otherTasks) != 0 {
		t.Fatalf("another session saw the task: %v", other)
	}

	callRPC(t, client, "session/task-kill", map[string]any{"sessionId": id, "taskId": taskID})
	deadline := time.Now().Add(10 * time.Second)
	for {
		listed = callRPC(t, client, "session/tasks", map[string]any{"sessionId": id})
		tasks, _ = listed["tasks"].([]any)
		if len(tasks) == 1 {
			if t0, _ := tasks[0].(map[string]any); t0["state"] != "running" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("task never left running: %v", listed)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// session/reload is the desktop's /reload: skill discovery re-runs
// mid-session and the summary reports the newly added skill.
func TestServerReloadE2E(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatSSE(w, "unused")
	}))
	defer srv.Close()
	testEnv(t, srv, nil)

	client := startPair(t, nil)
	created := callRPC(t, client, "session/create", nil)
	id := created["sessionId"].(string)

	// An unchanged tree reloads with no changes.
	r := callRPC(t, client, "session/reload", map[string]any{"sessionId": id})
	if r["ok"] != true || !strings.Contains(r["summary"].(string), "no changes") {
		t.Fatalf("reload = %v", r)
	}

	// A skill added after session creation shows up in the summary.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cwd, ".scode", "skills", "hot")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: hot\ndescription: added mid-session\n---\n\nBODY\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	r = callRPC(t, client, "session/reload", map[string]any{"sessionId": id})
	if r["ok"] != true || !strings.Contains(r["summary"].(string), "added: hot") {
		t.Fatalf("reload summary = %v", r)
	}
}

// The per-turn fork anchor: "k-th user turn from the end, reply
// included" must map to the right path-entry cut, and a turn count
// beyond what's on disk falls back to forking everything.
func TestForkUpToByTailTurns(t *testing.T) {
	cfgDir := t.TempDir()
	proj := t.TempDir()
	t.Setenv("SCODE_DIR", cfgDir)
	t.Chdir(proj)

	store, err := session.NewStore(session.DefaultRoot(cfgDir, proj))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create("turns", proj)
	if err != nil {
		t.Fatal(err)
	}
	sess.AppendAll([]llm.Message{ //nolint:errcheck
		{Role: llm.RoleSystem, Content: []llm.Block{llm.TextBlock("sys")}},
		{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("u1")}, TS: 1},
		{Role: llm.RoleAssistant, Content: []llm.Block{llm.TextBlock("a1")}, TS: 2},
		{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("u2")}, TS: 3},
		{Role: llm.RoleAssistant, Content: []llm.Block{llm.TextBlock("a2")}, TS: 4},
		{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("u3")}, TS: 5},
		{Role: llm.RoleAssistant, Content: []llm.Block{llm.TextBlock("a3")}, TS: 6},
	})
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	forkTexts := func(tailTurns int) []string {
		t.Helper()
		upto, err := forkUpToByTailTurns("turns", tailTurns)
		if err != nil {
			t.Fatalf("tailTurns=%d: %v", tailTurns, err)
		}
		id, err := forkSession("turns", upto)
		if err != nil {
			t.Fatalf("fork(upto=%d): %v", upto, err)
		}
		rec, err := store.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		var texts []string
		for _, e := range rec.Path() {
			if e.Msg != nil && len(e.Msg.Content) > 0 {
				texts = append(texts, e.Msg.Content[0].Text)
			}
		}
		return texts
	}

	// Last turn: everything through a3.
	if got := forkTexts(1); strings.Join(got, ",") != "sys,u1,a1,u2,a2,u3,a3" {
		t.Fatalf("tailTurns=1 forked %v", got)
	}
	// Second-to-last: through a2, u3/a3 dropped.
	if got := forkTexts(2); strings.Join(got, ",") != "sys,u1,a1,u2,a2" {
		t.Fatalf("tailTurns=2 forked %v", got)
	}
	// First turn: through a1 only.
	if got := forkTexts(3); strings.Join(got, ",") != "sys,u1,a1" {
		t.Fatalf("tailTurns=3 forked %v", got)
	}
	// Beyond the disk history: fork everything.
	if got := forkTexts(99); strings.Join(got, ",") != "sys,u1,a1,u2,a2,u3,a3" {
		t.Fatalf("tailTurns=99 forked %v", got)
	}
}
