package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/httpx"
)

// Proxy of the environment for every host but loopback, which a local browser always is
func loopbackProxy(req *http.Request) (*url.URL, error) {
	if httpx.Loopback(req.URL.Hostname()) {
		return nil, nil
	}
	return http.ProxyFromEnvironment(req)
}

// A websocket to a DevTools endpoint
type socket struct {
	conn  *websocket.Conn
	url   string
	write sync.Mutex
}

// A websocket failure: the connection dropped, was refused or never finished its handshake
type socketError struct{ err error }

func (e *socketError) Error() string { return e.err.Error() }
func (e *socketError) Unwrap() error { return e.err }

// Opens a websocket to the endpoint within the context's deadline, messages up to max bytes when max is positive
func dialSocket(ctx context.Context, endpoint string, maxMessage int64) (*socket, error) {
	dialer := websocket.Dialer{Proxy: loopbackProxy, HandshakeTimeout: 10 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, endpoint, nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		if resp != nil {
			return nil, &socketError{fmt.Errorf("server rejected WebSocket connection: HTTP %d", resp.StatusCode)}
		}
		return nil, &socketError{err}
	}
	if maxMessage > 0 {
		conn.SetReadLimit(maxMessage)
	}
	return &socket{conn: conn, url: endpoint}, nil
}

func (s *socket) send(ctx context.Context, payload []byte) error {
	s.write.Lock()
	defer s.write.Unlock()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	s.conn.SetWriteDeadline(deadline)
	if err := s.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		return &socketError{err}
	}
	return nil
}

// Next message, given up at the context's end
func (s *socket) read(ctx context.Context) ([]byte, error) {
	if deadline, ok := ctx.Deadline(); ok {
		s.conn.SetReadDeadline(deadline)
	} else {
		s.conn.SetReadDeadline(time.Time{})
	}
	stop := context.AfterFunc(ctx, func() { s.conn.SetReadDeadline(time.Now()) })
	defer stop()
	_, data, err := s.conn.ReadMessage()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &socketError{err}
	}
	return data, nil
}

// Closes with a normal close frame, the socket shut whatever the peer does
func (s *socket) close() {
	s.write.Lock()
	s.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	s.write.Unlock()
	s.conn.Close()
}

// A protocol error reply to one command
type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

func (e *cdpError) Error() string {
	if e.Data != "" {
		return fmt.Sprintf("CDP error %d: %s (%s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("CDP error %d: %s", e.Code, e.Message)
}

// A command whose connection dropped before its reply
var errClosed = errors.New("CDP connection closed before reply")

// One protocol event and the flattened session it fired on
type event struct {
	Method    string
	Params    json.RawMessage
	SessionID string
}

type message struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
}

type reply struct {
	result json.RawMessage
	err    error
}

// Commands and events of the DevTools protocol over one websocket
type cdpConn struct {
	ws       *socket
	next     atomic.Int64
	mu       sync.Mutex
	pending  map[int64]chan reply
	handlers map[int64]func(event)
	sub      int64
	done     chan struct{}
}

// Connects to a DevTools websocket and starts reading it
func dialCDP(ctx context.Context, endpoint string, maxMessage int64) (*cdpConn, error) {
	ws, err := dialSocket(ctx, endpoint, maxMessage)
	if err != nil {
		return nil, err
	}
	c := &cdpConn{ws: ws, pending: map[int64]chan reply{}, handlers: map[int64]func(event){}, done: make(chan struct{})}
	go c.loop()
	return c, nil
}

func (c *cdpConn) alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// Hands replies to the callers that wait on them and events to every handler, until the socket fails
func (c *cdpConn) loop() {
	for {
		data, err := c.ws.read(context.Background())
		if err != nil {
			break
		}
		var msg message
		if json.Unmarshal(data, &msg) != nil {
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
				ch <- reply{err: msg.Error}
			} else {
				ch <- reply{result: msg.Result}
			}
			continue
		}
		if msg.Method == "" {
			continue
		}
		c.mu.Lock()
		handlers := make([]func(event), 0, len(c.handlers))
		for _, h := range c.handlers {
			handlers = append(handlers, h)
		}
		c.mu.Unlock()
		for _, h := range handlers {
			h(event{Method: msg.Method, Params: msg.Params, SessionID: msg.SessionID})
		}
	}
	c.mu.Lock()
	for id, ch := range c.pending {
		ch <- reply{err: errClosed}
		delete(c.pending, id)
	}
	c.mu.Unlock()
	close(c.done)
}

// Sends one command, on the flattened session when given, and waits for its reply
func (c *cdpConn) call(ctx context.Context, method string, params any, session string) (json.RawMessage, error) {
	if !c.alive() {
		return nil, errClosed
	}
	id := c.next.Add(1)
	if params == nil {
		params = map[string]any{}
	}
	msg := map[string]any{"id": id, "method": method, "params": params}
	if session != "" {
		msg["sessionId"] = session
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	ch := make(chan reply, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	drop := func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}
	if err := c.ws.send(ctx, payload); err != nil {
		drop()
		return nil, err
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		drop()
		return nil, fmt.Errorf("%s: %w", method, ctx.Err())
	case <-c.done:
		return nil, errClosed
	}
}

// Command under a deadline of its own
func (c *cdpConn) callWithin(method string, params any, session string, timeout time.Duration) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.call(ctx, method, params, session)
}

// Adds an event handler and answers its removal
func (c *cdpConn) on(fn func(event)) func() {
	c.mu.Lock()
	c.sub++
	id := c.sub
	c.handlers[id] = fn
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.handlers, id)
		c.mu.Unlock()
	}
}

// Closes the connection and waits for its reader to end
func (c *cdpConn) close() {
	c.ws.close()
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
	}
}

// One entry of Target.getTargets
type targetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Attached bool   `json:"attached"`
}
