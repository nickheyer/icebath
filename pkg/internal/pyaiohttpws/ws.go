// Package pyaiohttpws is a client websocket with the receive, close and heartbeat behaviour of aiohttp
package pyaiohttpws

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Message kind with the numeric values of aiohttp WSMsgType
type MsgType int

const (
	Text    MsgType = 0x1
	Binary  MsgType = 0x2
	Close   MsgType = 0x8
	Closing MsgType = 0x100
	Closed  MsgType = 0x101
	Error   MsgType = 0x102
)

// Decimal value the way an IntEnum member prints through percent s
func (t MsgType) String() string { return strconv.Itoa(int(t)) }

const (
	closeOK       = 1000
	closeAbnormal = 1006
)

// One received message, Data a string for text, bytes for binary, the close code for close and the error for error
type Message struct {
	Type  MsgType
	Data  any
	Extra string
}

// Keywords of one ws_connect call
type Options struct {
	// Silence before a ping is sent, its pong due within half of it
	Heartbeat time.Duration
	// Wait for the peer's close frame on close, ten seconds when zero
	CloseTimeout time.Duration
	// Proxy settings read from the environment
	TrustEnv bool
	// Explicit proxy URL that wins over the environment
	Proxy     string
	TLSConfig *tls.Config
	Header    http.Header
	// Text aiohttp prints for the ssl argument in a connect error, default when empty
	SSLLabel string
}

// ClientConnectorError of a websocket that could not connect
type ConnectorError struct{ Text string }

func (e *ConnectorError) Error() string      { return e.Text }
func (e *ConnectorError) PyTypeName() string { return "ClientConnectorError" }

// WSServerHandshakeError of an upgrade the server refused
type HandshakeError struct {
	Status int
	URL    string
}

func (e *HandshakeError) Error() string {
	return fmt.Sprintf("%d, message='Invalid response status', url=%s", e.Status, pyrepr.Repr(e.URL))
}
func (e *HandshakeError) PyTypeName() string { return "WSServerHandshakeError" }

// ClientConnectionResetError of a write on a websocket that is closing or closed
type ConnectionResetError struct{}

func (e *ConnectionResetError) Error() string      { return "Cannot write to closing transport" }
func (e *ConnectionResetError) PyTypeName() string { return "ClientConnectionResetError" }

// ServerTimeoutError of a heartbeat whose pong never arrived within the pong window
type ServerTimeoutError struct{ PongSeconds float64 }

func (e *ServerTimeoutError) Error() string {
	return fmt.Sprintf("No PONG received after %s seconds", pyrepr.Float(e.PongSeconds))
}
func (e *ServerTimeoutError) PyTypeName() string { return "ServerTimeoutError" }

// One open client websocket with its reader, heartbeat and close state
type Conn struct {
	ws           *websocket.Conn
	heartbeat    time.Duration
	closeTimeout time.Duration

	writeMu sync.Mutex
	mu      sync.Mutex
	closed  bool
	closing bool
	local   bool
	seen    bool
	code    int
	pending *time.Timer
	idle    *time.Timer

	msgs     chan Message
	done     chan struct{}
	quit     chan struct{}
	quitOnce sync.Once
	terminal sync.Once
}

// Host and port of a websocket URL with the scheme default port
func hostPort(rawURL string) (string, string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL, ""
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "wss" || u.Scheme == "https" {
			port = "443"
		}
	}
	return u.Hostname(), port
}

// strerror of the OSError asyncio raises for a failed connection
func connectReason(err error) string {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		if dns.IsTemporary || dns.IsTimeout {
			return "Temporary failure in name resolution"
		}
		return "Name or service not known"
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Addr != nil {
		if tcp, ok := op.Addr.(*net.TCPAddr); ok {
			return fmt.Sprintf("Connect call failed ('%s', %d)", tcp.IP.String(), tcp.Port)
		}
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err.Error()
	}
	return err.Error()
}

// Opens a client websocket the way a session ws_connect does, the upgrade bounded by the session total timeout
func Connect(ctx context.Context, rawURL string, opts Options) (*Conn, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 300 * time.Second, TLSClientConfig: opts.TLSConfig,
		NetDialContext: (&net.Dialer{Timeout: 30 * time.Second}).DialContext}
	if opts.TrustEnv {
		dialer.Proxy = http.ProxyFromEnvironment
	}
	if opts.Proxy != "" {
		proxyURL, err := url.Parse(opts.Proxy)
		if err != nil {
			return nil, &ConnectorError{Text: err.Error()}
		}
		dialer.Proxy = http.ProxyURL(proxyURL)
	}
	ws, resp, err := dialer.DialContext(ctx, rawURL, opts.Header)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if resp != nil && errors.Is(err, websocket.ErrBadHandshake) {
			return nil, &HandshakeError{Status: resp.StatusCode, URL: rawURL}
		}
		host, port := hostPort(rawURL)
		label := opts.SSLLabel
		if label == "" {
			label = "default"
		}
		return nil, &ConnectorError{Text: fmt.Sprintf("Cannot connect to host %s:%s ssl:%s [%s]", host, port, label, connectReason(err))}
	}
	c := &Conn{ws: ws, heartbeat: opts.Heartbeat, closeTimeout: opts.CloseTimeout, msgs: make(chan Message, 64),
		done: make(chan struct{}), quit: make(chan struct{})}
	if c.closeTimeout == 0 {
		c.closeTimeout = 10 * time.Second
	}
	ws.SetPingHandler(func(data string) error {
		c.resetHeartbeat()
		c.writeMu.Lock()
		defer c.writeMu.Unlock()
		return ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})
	ws.SetPongHandler(func(string) error {
		c.resetHeartbeat()
		return nil
	})
	ws.SetCloseHandler(func(int, string) error { return nil })
	c.resetHeartbeat()
	go c.readLoop()
	return c, nil
}

// Queues one message for the receiver, giving up once the websocket is torn down
func (c *Conn) deliver(m Message) {
	select {
	case c.msgs <- m:
	case <-c.quit:
	}
}

// Queues the one message that ends the stream, later ones dropped
func (c *Conn) finish(m Message) {
	c.terminal.Do(func() {
		c.deliver(m)
		close(c.msgs)
	})
}

// Writes a close frame under the write lock
func (c *Conn) writeClose(code int) {
	c.writeMu.Lock()
	_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""), time.Now().Add(5*time.Second))
	c.writeMu.Unlock()
}

// Drops the connection and releases a reader blocked on delivery
func (c *Conn) teardown() {
	c.quitOnce.Do(func() { close(c.quit) })
	c.ws.Close()
}

// Reads frames until the connection ends, turning the end into its close, closed or error message
func (c *Conn) readLoop() {
	defer close(c.done)
	for {
		kind, data, err := c.ws.ReadMessage()
		if err == nil {
			c.resetHeartbeat()
			if kind == websocket.TextMessage {
				c.deliver(Message{Type: Text, Data: string(data)})
			} else {
				c.deliver(Message{Type: Binary, Data: data})
			}
			continue
		}
		c.stopHeartbeat()
		var closeErr *websocket.CloseError
		peerClosed := errors.As(err, &closeErr) && closeErr.Code != websocket.CloseAbnormalClosure
		c.mu.Lock()
		wasClosed := c.closed
		c.closed = true
		switch {
		case peerClosed:
			c.closing, c.code = true, closeErr.Code
		case !wasClosed:
			c.code = closeAbnormal
		}
		c.mu.Unlock()
		if peerClosed {
			if !wasClosed {
				c.writeClose(closeErr.Code)
			}
			c.finish(Message{Type: Close, Data: closeErr.Code, Extra: closeErr.Text})
		} else {
			c.finish(Message{Type: Closed})
		}
		c.teardown()
		return
	}
}

// Restarts the silence timer that sends the next heartbeat ping and cancels a pending pong deadline
func (c *Conn) resetHeartbeat() {
	if c.heartbeat <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending != nil {
		c.pending.Stop()
		c.pending = nil
	}
	if c.idle != nil {
		c.idle.Stop()
	}
	if c.closed {
		return
	}
	c.idle = time.AfterFunc(c.heartbeat, c.sendHeartbeat)
}

// Cancels the heartbeat timers
func (c *Conn) stopHeartbeat() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending != nil {
		c.pending.Stop()
		c.pending = nil
	}
	if c.idle != nil {
		c.idle.Stop()
		c.idle = nil
	}
}

// Sends the heartbeat ping and arms the pong deadline at half the heartbeat
func (c *Conn) sendHeartbeat() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.pending = time.AfterFunc(c.heartbeat/2, c.pongNotReceived)
	c.mu.Unlock()
	c.writeMu.Lock()
	_ = c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
	c.writeMu.Unlock()
}

// Closes the connection as abnormal and reports the missing pong to the receiver
func (c *Conn) pongNotReceived() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed, c.code = true, closeAbnormal
	c.mu.Unlock()
	c.finish(Message{Type: Error, Data: &ServerTimeoutError{PongSeconds: (c.heartbeat / 2).Seconds()}})
	c.teardown()
}

// Whether the websocket is closed as its owner has seen it, by a local close or a received end of stream
func (c *Conn) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen
}

// Marks the end of the stream as seen by the receiver and returns the message
func (c *Conn) observed(m Message) Message {
	if m.Type == Close || m.Type == Closed || m.Type == Error {
		c.mu.Lock()
		c.seen = true
		c.mu.Unlock()
	}
	return m
}

// Close code of the connection, zero while none is known
func (c *Conn) CloseCode() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.code
}

// Next message, the context error when it ends the wait and a closed message after the stream ended
func (c *Conn) Receive(ctx context.Context) (Message, error) {
	c.mu.Lock()
	local := c.local
	c.mu.Unlock()
	if local {
		return Message{Type: Closed}, nil
	}
	select {
	case m, ok := <-c.msgs:
		if !ok {
			return c.observed(Message{Type: Closed}), nil
		}
		return c.observed(m), nil
	default:
	}
	select {
	case m, ok := <-c.msgs:
		if !ok {
			return c.observed(Message{Type: Closed}), nil
		}
		return c.observed(m), nil
	case <-ctx.Done():
		c.mu.Lock()
		c.code = closeAbnormal
		c.mu.Unlock()
		return Message{}, ctx.Err()
	}
}

// Sends one text frame, the reset error when the websocket is closing or closed
func (c *Conn) SendStr(text string) error {
	c.mu.Lock()
	unusable := c.closed || c.closing
	c.mu.Unlock()
	if unusable {
		return &ConnectionResetError{}
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.ws.WriteMessage(websocket.TextMessage, []byte(text)); err != nil {
		return &ConnectionResetError{}
	}
	return nil
}

// Sends the close frame, waits the close timeout for the peer to end the stream and drops the connection
func (c *Conn) Close(ctx context.Context) bool {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false
	}
	c.closed, c.local, c.seen = true, true, true
	if c.code == 0 {
		c.code = closeOK
	}
	c.mu.Unlock()
	c.stopHeartbeat()
	c.writeClose(closeOK)
	timer := time.NewTimer(c.closeTimeout)
	defer timer.Stop()
	select {
	case <-c.done:
	case <-timer.C:
	case <-ctx.Done():
	}
	c.teardown()
	return true
}
