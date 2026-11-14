package pywebsockets

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Keyword arguments of websockets connect, a zero field taking the library default
type ConnectOptions struct {
	OpenTimeout        time.Duration
	CloseTimeout       time.Duration
	PingInterval       time.Duration
	PingTimeout        time.Duration
	MaxSize            int64
	HappyEyeballsDelay time.Duration
	// Keepalive pings off, the ping_interval None of websockets connect
	NoKeepalive bool
}

func (o ConnectOptions) withDefaults() ConnectOptions {
	if o.OpenTimeout == 0 {
		o.OpenTimeout = 10 * time.Second
	}
	if o.CloseTimeout == 0 {
		o.CloseTimeout = 10 * time.Second
	}
	if o.PingInterval == 0 {
		o.PingInterval = 20 * time.Second
	}
	if o.PingTimeout == 0 {
		o.PingTimeout = 20 * time.Second
	}
	if o.MaxSize == 0 {
		o.MaxSize = 1 << 20
	}
	return o
}

// URI the client refuses before dialing, the websockets InvalidURI exception
type InvalidURI struct{ URI, Msg string }

func (e *InvalidURI) Error() string      { return e.URI + " isn't a valid URI: " + e.Msg }
func (e *InvalidURI) PyTypeName() string { return "InvalidURI" }

// Opening handshake that outlasted the open timeout, the TimeoutError websockets connect raises
type OpenTimeoutError struct{}

func (e *OpenTimeoutError) Error() string      { return "timed out during opening handshake" }
func (e *OpenTimeoutError) PyTypeName() string { return "TimeoutError" }

// Failed TCP connect carrying the str of the OSError asyncio create_connection raises
type DialError struct{ Text string }

func (e *DialError) Error() string      { return e.Text }
func (e *DialError) PyTypeName() string { return "OSError" }

// Checks the scheme, host and fragment rules websockets parse_uri enforces
func validateURI(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return &InvalidURI{URI: rawURL, Msg: "scheme isn't ws or wss"}
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return &InvalidURI{URI: rawURL, Msg: "scheme isn't ws or wss"}
	}
	if parsed.Hostname() == "" {
		return &InvalidURI{URI: rawURL, Msg: "hostname isn't provided"}
	}
	if parsed.Fragment != "" {
		return &InvalidURI{URI: rawURL, Msg: "fragment identifier is meaningless"}
	}
	return nil
}

// Address tuple repr asyncio prints in a connect failure
func addressRepr(ip net.IP, port int) string {
	if ip.To4() != nil {
		return fmt.Sprintf("(%s, %d)", pyrepr.StrRepr(ip.String()), port)
	}
	return fmt.Sprintf("(%s, %d, 0, 0)", pyrepr.StrRepr(ip.String()), port)
}

// str of the OSError one failed address attempt raises
func attemptErrorText(err error, ip net.IP, port int) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Sprintf("[Errno %d] Connect call failed %s", int(errno), addressRepr(ip, port))
	}
	return pyhttpx.OSErrorText(err)
}

// Dials each resolved address in order, staggered by the delay when set, and joins the failures like asyncio create_connection
func addressDialer(stagger time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, portText, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		port, err := strconv.Atoi(portText)
		if err != nil {
			return nil, err
		}
		var ips []net.IP
		if ip := net.ParseIP(host); ip != nil {
			ips = []net.IP{ip}
		} else {
			resolved, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, r := range resolved {
				ips = append(ips, r.IP)
			}
		}
		if len(ips) == 0 {
			return nil, &DialError{Text: "getaddrinfo() returned empty list"}
		}
		type outcome struct {
			index int
			conn  net.Conn
			err   error
		}
		actx, cancel := context.WithCancel(ctx)
		defer cancel()
		results := make(chan outcome, len(ips))
		attempt := func(i int) {
			var d net.Dialer
			conn, err := d.DialContext(actx, network, net.JoinHostPort(ips[i].String(), portText))
			results <- outcome{i, conn, err}
		}
		texts := make([]string, len(ips))
		started, finished := 0, 0
		var timer <-chan time.Time
		start := func() {
			go attempt(started)
			started++
			timer = nil
			if stagger > 0 && started < len(ips) {
				timer = time.After(stagger)
			}
		}
		start()
		for finished < len(ips) {
			select {
			case <-timer:
				start()
			case r := <-results:
				finished++
				if r.err == nil {
					go func(pending int) {
						for ; pending > 0; pending-- {
							if late := <-results; late.conn != nil {
								late.conn.Close()
							}
						}
					}(started - finished)
					return r.conn, nil
				}
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				texts[r.index] = attemptErrorText(r.err, ips[r.index], port)
				if started < len(ips) && started == finished {
					start()
				}
			}
		}
		same := true
		for _, t := range texts {
			same = same && t == texts[0]
		}
		if same {
			return nil, &DialError{Text: texts[0]}
		}
		return nil, &DialError{Text: "Multiple exceptions: " + strings.Join(texts, ", ")}
	}
}

// One open client connection with its close record, reader and keepalive
type Conn struct {
	ws           *websocket.Conn
	closeTimeout time.Duration
	messages     chan any
	closing      chan struct{}
	done         chan struct{}
	pongs        chan string
	writeMu      sync.Mutex
	mu           sync.Mutex
	sent, rcvd   *Close
	rcvdThenSent bool
	closingOnce  sync.Once
	doneOnce     sync.Once
}

// Opens a client connection the way websockets connect does, the opening handshake bounded by the open timeout
func Connect(ctx context.Context, rawURL string, opts ConnectOptions) (*Conn, error) {
	opts = opts.withDefaults()
	if err := validateURI(rawURL); err != nil {
		return nil, err
	}
	dialer := websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: opts.OpenTimeout, EnableCompression: true,
		NetDialContext: addressDialer(opts.HappyEyeballsDelay)}
	dctx, cancel := context.WithTimeout(ctx, opts.OpenTimeout)
	defer cancel()
	ws, resp, err := dialer.DialContext(dctx, rawURL, nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var netErr net.Error
		var dialErr *DialError
		switch {
		case resp != nil && errors.Is(err, websocket.ErrBadHandshake):
			return nil, &InvalidStatus{StatusCode: resp.StatusCode}
		case errors.As(err, &dialErr):
			return nil, dialErr
		case dctx.Err() != nil || (errors.As(err, &netErr) && netErr.Timeout()):
			return nil, &OpenTimeoutError{}
		}
		return nil, &DialError{Text: pyhttpx.OSErrorText(err)}
	}
	ws.SetReadLimit(opts.MaxSize)
	c := &Conn{ws: ws, closeTimeout: opts.CloseTimeout, messages: make(chan any, 16), closing: make(chan struct{}),
		done: make(chan struct{}), pongs: make(chan string, 1)}
	ws.SetPongHandler(func(data string) error {
		select {
		case c.pongs <- data:
		default:
		}
		return nil
	})
	ws.SetCloseHandler(func(code int, text string) error {
		c.mu.Lock()
		c.rcvd = &Close{Code: code, Reason: text}
		echo := c.sent == nil
		if echo {
			c.sent, c.rcvdThenSent = &Close{Code: code, Reason: text}, true
		}
		c.mu.Unlock()
		if echo {
			_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, text), time.Now().Add(time.Second))
		}
		return nil
	})
	go c.readLoop()
	if !opts.NoKeepalive {
		go c.keepalive(opts.PingInterval, opts.PingTimeout)
	}
	return c, nil
}

// Queues every received message until the connection ends, dropping them once a close is under way
func (c *Conn) readLoop() {
	defer close(c.messages)
	defer c.markDone()
	for {
		kind, data, err := c.ws.ReadMessage()
		if err != nil {
			if errors.Is(err, websocket.ErrReadLimit) {
				c.mu.Lock()
				if c.sent == nil {
					c.sent = &Close{Code: 1009}
				}
				c.mu.Unlock()
			}
			_ = c.ws.Close()
			return
		}
		var message any = data
		if kind == websocket.TextMessage {
			message = string(data)
		}
		select {
		case c.messages <- message:
		case <-c.closing:
		case <-c.done:
			return
		}
	}
}

func (c *Conn) markDone() { c.doneOnce.Do(func() { close(c.done) }) }

// Pings at the interval less the last latency and fails the connection 1011 when a pong misses the timeout
func (c *Conn) keepalive(interval, timeout time.Duration) {
	latency := time.Duration(0)
	for {
		timer := time.NewTimer(interval - latency)
		select {
		case <-c.done:
			timer.Stop()
			return
		case <-timer.C:
		}
		payload := make([]byte, 4)
		_, _ = rand.Read(payload)
		sentAt := time.Now()
		if err := c.ws.WriteControl(websocket.PingMessage, payload, time.Now().Add(timeout)); err != nil {
			return
		}
		deadline := time.NewTimer(timeout)
	waitPong:
		for {
			select {
			case <-c.done:
				deadline.Stop()
				return
			case data := <-c.pongs:
				if data == string(payload) {
					latency = time.Since(sentAt)
					deadline.Stop()
					break waitPong
				}
			case <-deadline.C:
				c.fail(1011, "keepalive ping timeout")
				return
			}
		}
	}
}

// Sends a close frame once, records it and drops the TCP connection
func (c *Conn) fail(code int, reason string) {
	c.mu.Lock()
	first := c.sent == nil
	if first {
		c.sent = &Close{Code: code, Reason: reason}
	}
	c.mu.Unlock()
	if first {
		_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	}
	c.markDone()
	_ = c.ws.Close()
}

// ConnectionClosed describing the close frames this connection received and sent
func (c *Conn) closedError() *ConnectionClosed {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &ConnectionClosed{Rcvd: c.rcvd, Sent: c.sent, RcvdThenSent: c.rcvdThenSent}
}

// Next message, a text frame as a string and a binary frame as bytes, ConnectionClosed once the connection ended
func (c *Conn) Recv(ctx context.Context) (any, error) {
	select {
	case message, ok := <-c.messages:
		if !ok {
			return nil, c.closedError()
		}
		return message, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Sends one text frame, ConnectionClosed when the connection already ended or the write fails
func (c *Conn) Send(message string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.done:
		return c.closedError()
	default:
	}
	if err := c.ws.WriteMessage(websocket.TextMessage, []byte(message)); err != nil {
		c.markDone()
		_ = c.ws.Close()
		return c.closedError()
	}
	return nil
}

// Sends one ping frame with a random payload, ConnectionClosed when the connection already ended or the write fails
func (c *Conn) Ping() error {
	select {
	case <-c.done:
		return c.closedError()
	default:
	}
	payload := make([]byte, 4)
	_, _ = rand.Read(payload)
	if err := c.ws.WriteControl(websocket.PingMessage, payload, time.Now().Add(c.closeTimeout)); err != nil {
		return c.closedError()
	}
	return nil
}

// Runs the closing handshake with code 1000 bounded by the close timeout, then drops the TCP connection
func (c *Conn) Close() {
	c.closingOnce.Do(func() { close(c.closing) })
	c.mu.Lock()
	first := c.sent == nil
	if first {
		c.sent = &Close{Code: 1000}
	}
	c.mu.Unlock()
	if first {
		_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, ""), time.Now().Add(c.closeTimeout))
	}
	timer := time.NewTimer(c.closeTimeout)
	select {
	case <-c.done:
	case <-timer.C:
	}
	timer.Stop()
	c.markDone()
	_ = c.ws.Close()
}

// Whether an error is the clean closure that ends iteration over a connection without raising
func IsClosedOK(err error) bool {
	var closed *ConnectionClosed
	return errors.As(err, &closed) && closed.PyTypeName() == "ConnectionClosedOK"
}
