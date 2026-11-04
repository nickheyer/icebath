package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// CDPError is a protocol error reply to one command
type CDPError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

func (e *CDPError) Error() string {
	if e.Data != "" {
		return fmt.Sprintf("CDP error %d: %s (%s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("CDP error %d: %s", e.Code, e.Message)
}

// ErrCDPClosed reports a command whose connection dropped before its reply
var ErrCDPClosed = errors.New("CDP connection closed before reply")

// CDPEvent is one protocol event with the flattened session it fired on
type CDPEvent struct {
	Method    string
	Params    json.RawMessage
	SessionID string
}

type cdpMessage struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *CDPError       `json:"error,omitempty"`
}

type cdpReply struct {
	result json.RawMessage
	err    error
}

// CDPConn multiplexes commands and events over one DevTools websocket
type CDPConn struct {
	ws       *WSConn
	nextID   atomic.Int64
	mu       sync.Mutex
	pending  map[int64]chan cdpReply
	handlers map[int64]func(CDPEvent)
	nextSub  int64
	done     chan struct{}
	err      error
	closing  atomic.Bool
}

// DialCDP connects to a DevTools websocket URL and starts the reader
func DialCDP(ctx context.Context, wsURL string, maxMessage int64) (*CDPConn, error) {
	ws, err := DialWS(ctx, wsURL, WSOptions{MaxMessageSize: maxMessage})
	if err != nil {
		return nil, err
	}
	return NewCDPConn(ws), nil
}

// NewCDPConn wraps an open websocket and starts dispatching its frames
func NewCDPConn(ws *WSConn) *CDPConn {
	c := &CDPConn{ws: ws, pending: map[int64]chan cdpReply{}, handlers: map[int64]func(CDPEvent){}, done: make(chan struct{})}
	go c.readLoop()
	return c
}

// URL returns the websocket endpoint of this connection
func (c *CDPConn) URL() string { return c.ws.URL() }

// Done closes once the connection is gone
func (c *CDPConn) Done() <-chan struct{} { return c.done }

// Err is the reason the connection ended, nil while it runs
func (c *CDPConn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Alive reports whether the reader still runs
func (c *CDPConn) Alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// Dispatches replies to their waiting callers and events to every subscriber until the socket fails
func (c *CDPConn) readLoop() {
	var failure error
	for {
		_, payload, err := c.ws.ReadMessage(context.Background())
		if err != nil {
			failure = err
			break
		}
		var msg cdpMessage
		if json.Unmarshal(payload, &msg) != nil {
			continue
		}
		if msg.ID != 0 {
			c.mu.Lock()
			ch := c.pending[msg.ID]
			delete(c.pending, msg.ID)
			c.mu.Unlock()
			if ch == nil {
				continue
			}
			if msg.Error != nil {
				ch <- cdpReply{err: msg.Error}
			} else {
				ch <- cdpReply{result: msg.Result}
			}
			continue
		}
		if msg.Method == "" {
			continue
		}
		c.mu.Lock()
		handlers := make([]func(CDPEvent), 0, len(c.handlers))
		for _, h := range c.handlers {
			handlers = append(handlers, h)
		}
		c.mu.Unlock()
		ev := CDPEvent{Method: msg.Method, Params: msg.Params, SessionID: msg.SessionID}
		for _, h := range handlers {
			h(ev)
		}
	}
	c.mu.Lock()
	if c.closing.Load() {
		c.err = errors.New("CDP connection closed")
	} else {
		c.err = failure
	}
	for id, ch := range c.pending {
		ch <- cdpReply{err: ErrCDPClosed}
		delete(c.pending, id)
	}
	c.mu.Unlock()
	close(c.done)
}

// Call sends one command, on the flattened session when given, and waits for its reply
func (c *CDPConn) Call(ctx context.Context, method string, params any, sessionID string) (json.RawMessage, error) {
	if !c.Alive() {
		return nil, ErrCDPClosed
	}
	id := c.nextID.Add(1)
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	} else {
		msg["params"] = map[string]any{}
	}
	if sessionID != "" {
		msg["sessionId"] = sessionID
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	ch := make(chan cdpReply, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.ws.WriteText(ctx, payload); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("%s: %w", method, ctx.Err())
	case <-c.done:
		return nil, ErrCDPClosed
	}
}

// CallTimeout runs Call under a fresh deadline
func (c *CDPConn) CallTimeout(method string, params any, sessionID string, timeout time.Duration) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.Call(ctx, method, params, sessionID)
}

// CallInto decodes a command's result into out
func (c *CDPConn) CallInto(ctx context.Context, method string, params any, sessionID string, out any) error {
	raw, err := c.Call(ctx, method, params, sessionID)
	if err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// OnEvent registers an event handler and returns its remover
func (c *CDPConn) OnEvent(fn func(CDPEvent)) func() {
	c.mu.Lock()
	c.nextSub++
	id := c.nextSub
	c.handlers[id] = fn
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.handlers, id)
		c.mu.Unlock()
	}
}

// Close ends the connection with a normal close handshake, forcing the socket shut after two seconds
func (c *CDPConn) Close() error {
	c.closing.Store(true)
	err := c.ws.SendClose(CloseNormal, "")
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		c.ws.ForceClose()
		<-c.done
	}
	return err
}

// TargetInfo is one entry of Target.getTargets
type TargetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Attached bool   `json:"attached"`
	OpenerID string `json:"openerId,omitempty"`
}

// Targets lists the browser's targets
func (c *CDPConn) Targets(ctx context.Context) ([]TargetInfo, error) {
	var out struct {
		TargetInfos []TargetInfo `json:"targetInfos"`
	}
	if err := c.CallInto(ctx, "Target.getTargets", nil, "", &out); err != nil {
		return nil, err
	}
	return out.TargetInfos, nil
}

// Attach opens a flattened session on a target and returns its session id
func (c *CDPConn) Attach(ctx context.Context, targetID string) (string, error) {
	var out struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.CallInto(ctx, "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, "", &out); err != nil {
		return "", err
	}
	if out.SessionID == "" {
		return "", errors.New("Target.attachToTarget did not return a sessionId")
	}
	return out.SessionID, nil
}

// Detach closes a flattened session
func (c *CDPConn) Detach(ctx context.Context, sessionID string) error {
	_, err := c.Call(ctx, "Target.detachFromTarget", map[string]any{"sessionId": sessionID}, "")
	return err
}

// CreateTarget opens a new page target and returns its id
func (c *CDPConn) CreateTarget(ctx context.Context, url string, background bool) (string, error) {
	var out struct {
		TargetID string `json:"targetId"`
	}
	params := map[string]any{"url": url}
	if background {
		params["background"] = true
	}
	if err := c.CallInto(ctx, "Target.createTarget", params, "", &out); err != nil {
		return "", err
	}
	return out.TargetID, nil
}
