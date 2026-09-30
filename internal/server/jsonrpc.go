// Package server exposes scode's agent as a headless session service
// over JSON-RPC 2.0 with newline-delimited framing on stdio (the Codex
// app-server / ACP shape; design: docs/design-permission-plan-desktop.md
// §四). stdout carries framed JSON-RPC only — all logging goes to
// stderr. The transport is full-duplex: client→server methods,
// server→client notifications (event streams), and server→client
// requests (approvals the client answers).
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// ProtocolVersion is the wire contract version; initialize handshakes
// it so shell and binary can evolve independently.
const ProtocolVersion = 1

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// rpcMessage is the single envelope for requests, responses, and
// notifications (a request has id+method, a notification only method,
// a response only id).
type rpcMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

const (
	codeParseError     = -32700
	codeMethodNotFound = -32601
	codeInternal       = -32603
)

// Conn is one full-duplex JSON-RPC channel. Handlers answer incoming
// requests; Call issues reverse (server→client) requests and blocks
// until the peer answers or ctx cancels.
type Conn struct {
	w   io.Writer
	wmu sync.Mutex // one writer at a time: a frame never interleaves

	mu      sync.Map // int64 → chan rpcMessage (pending reverse calls)
	nextID  atomic.Int64
	wg      sync.WaitGroup // in-flight request handlers (drained at EOF)
	onReq   func(ctx context.Context, method string, params json.RawMessage) (any, error)
	onNotif func(method string, params json.RawMessage)
}

// NewConn wires the channel: w is the frame sink (set once, before
// any Call/Notify), onReq answers incoming requests, onNotif receives
// notifications (may be nil).
func NewConn(w io.Writer, onReq func(ctx context.Context, method string, params json.RawMessage) (any, error),
	onNotif func(method string, params json.RawMessage)) *Conn {
	return &Conn{w: w, onReq: onReq, onNotif: onNotif}
}

// Serve reads NDJSON frames from r and dispatches until EOF or ctx
// cancellation. Incoming requests run in their own goroutines so a
// long-running method never blocks the read loop (a reverse approval
// request must flow while a prompt runs).
func (c *Conn) Serve(ctx context.Context, r io.Reader) error {
	// Peer-gone semantics: EOF cancels the handler context, so handlers
	// blocked in reverse calls unblock and the EOF drain terminates.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 64<<20)
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m rpcMessage
		if err := json.Unmarshal(line, &m); err != nil {
			c.write(rpcMessage{JSONRPC: "2.0", Error: &rpcError{Code: codeParseError, Message: err.Error()}})
			continue
		}
		switch {
		case m.Method != "" && m.ID != nil: // incoming request
			c.wg.Add(1)
			go func(m rpcMessage) {
				defer c.wg.Done()
				result, err := c.onReq(ctx, m.Method, m.Params)
				resp := rpcMessage{JSONRPC: "2.0", ID: m.ID}
				if err != nil {
					resp.Error = &rpcError{Code: codeInternal, Message: err.Error()}
				} else if result != nil {
					raw, merr := json.Marshal(result)
					if merr != nil {
						resp.Error = &rpcError{Code: codeInternal, Message: merr.Error()}
					} else {
						resp.Result = raw
					}
				} else {
					resp.Result = json.RawMessage(`{}`)
				}
				c.write(resp)
			}(m)
		case m.Method != "": // notification
			if c.onNotif != nil {
				c.onNotif(m.Method, m.Params)
			}
		case m.ID != nil: // response to a reverse call
			var id int64
			if err := json.Unmarshal(*m.ID, &id); err == nil {
				if ch, ok := c.mu.LoadAndDelete(id); ok {
					ch.(chan rpcMessage) <- m
				}
			}
		}
	}
	// EOF: drain in-flight handlers so their responses are written
	// before the caller tears down (stdin-close must not truncate the
	// last answer).
	c.wg.Wait()
	return sc.Err()
}

// Call sends a reverse request and blocks for the peer's response.
func (c *Conn) Call(ctx context.Context, method string, params, resultOut any) error {
	id := c.nextID.Add(1)
	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}
	rawID, _ := json.Marshal(id)
	ch := make(chan rpcMessage, 1)
	c.mu.Store(id, ch)
	defer c.mu.Delete(id)
	if err := c.write(rpcMessage{JSONRPC: "2.0", ID: (*json.RawMessage)(&rawID), Method: method, Params: rawParams}); err != nil {
		return err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return fmt.Errorf("%s", resp.Error.Message)
		}
		if resultOut != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, resultOut)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Notify sends a server→client notification (no response expected).
func (c *Conn) Notify(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(rpcMessage{JSONRPC: "2.0", Method: method, Params: raw})
}

func (c *Conn) write(m rpcMessage) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(append(b, '\n'))
	return err
}
