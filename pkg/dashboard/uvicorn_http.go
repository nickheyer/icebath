package dashboard

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http/httputil"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// One client connection speaking HTTP/1.1 until it closes or upgrades to a websocket
type httpConn struct {
	srv          *UvicornServer
	conn         net.Conn
	br           *bufio.Reader
	mu           sync.Mutex
	cycle        *requestCycle
	ws           *wsSession
	shuttingDown bool
}

func newHTTPConn(srv *UvicornServer, c net.Conn) *httpConn {
	return &httpConn{srv: srv, conn: c, br: bufio.NewReaderSize(c, 64*1024)}
}

// Closes an idle connection or lets the running request finish without keep alive
func (h *httpConn) shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.shuttingDown = true
	if h.ws != nil {
		go h.ws.shutdown()
		return
	}
	if h.cycle == nil || h.cycle.responseComplete() {
		h.conn.Close()
		return
	}
	h.cycle.keepAlive = false
}

// Address of a socket end as the ASGI host and port pair
func addrOf(a net.Addr) *Address {
	if tcp, ok := a.(*net.TCPAddr); ok {
		return &Address{Host: tcp.IP.String(), Port: tcp.Port}
	}
	return nil
}

// Parsed request head, its raw headers in order and lowercased
type requestHead struct {
	method, target, proto string
	headers               []Header
}

// Reads one request head, the error for a malformed request line or header
func readRequestHead(br *bufio.Reader) (*requestHead, error) {
	tp := textproto.NewReader(br)
	line, err := tp.ReadLine()
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 || !strings.HasPrefix(parts[2], "HTTP/1.") {
		return nil, errInvalidRequest
	}
	head := &requestHead{method: parts[0], target: parts[1], proto: parts[2]}
	for {
		l, err := tp.ReadContinuedLine()
		if err != nil {
			return nil, errInvalidRequest
		}
		if l == "" {
			break
		}
		name, value, ok := strings.Cut(l, ":")
		if !ok || name == "" || strings.ContainsAny(name, " \t") {
			return nil, errInvalidRequest
		}
		head.headers = append(head.headers, Header{strings.ToLower(name), strings.Trim(value, " \t")})
	}
	return head, nil
}

var errInvalidRequest = errors.New("Invalid HTTP request received.")

func headValue(hs []Header, name string) (string, bool) {
	for _, h := range hs {
		if h[0] == name {
			return h[1], true
		}
	}
	return "", false
}

// Serves requests in turn, handing a websocket upgrade to the websocket protocol
func (h *httpConn) serve() {
	defer h.conn.Close()
	for {
		h.conn.SetReadDeadline(time.Now().Add(h.srv.cfg.TimeoutKeepAlive))
		head, err := readRequestHead(h.br)
		h.conn.SetReadDeadline(time.Time{})
		if err != nil {
			if errors.Is(err, errInvalidRequest) {
				h.sendInvalid()
			}
			return
		}
		rawPath, query, _ := strings.Cut(head.target, "?")
		if strings.HasPrefix(rawPath, "http://") || strings.HasPrefix(rawPath, "https://") {
			if u, perr := pyurl.Split(head.target); perr == nil {
				rawPath, query = u.Path, u.Query
			}
		}
		if !isASCII(rawPath) {
			h.sendInvalid()
			return
		}
		scope := &Scope{Type: "http", HTTPVersion: strings.TrimPrefix(head.proto, "HTTP/"), Method: head.method,
			Scheme: "http", Path: pyurl.Unquote(rawPath), RawPath: rawPath, QueryString: query, Headers: head.headers,
			Client: addrOf(h.conn.RemoteAddr()), Server: addrOf(h.conn.LocalAddr()),
			Extensions: map[string]any{}, State: copyState(h.srv.lifespan.state)}
		if isWebSocketUpgrade(head) {
			scope.Type, scope.Scheme = "websocket", "ws"
			scope.Method = ""
			serveWebSocket(h, scope, head)
			return
		}
		cycle := newRequestCycle(h, scope, head)
		h.mu.Lock()
		h.cycle = cycle
		stop := h.shuttingDown
		h.mu.Unlock()
		if stop {
			return
		}
		cycle.run()
		if !cycle.keepAlive || !cycle.drainBody() {
			return
		}
		h.mu.Lock()
		h.cycle = nil
		stop = h.shuttingDown
		h.mu.Unlock()
		if stop {
			return
		}
	}
}

func copyState(s map[string]any) map[string]any {
	out := make(map[string]any, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}

// Whether the head asks for a websocket upgrade
func isWebSocketUpgrade(head *requestHead) bool {
	up, _ := headValue(head.headers, "upgrade")
	conn, _ := headValue(head.headers, "connection")
	hasUpgrade := false
	for _, tok := range strings.Split(strings.ToLower(conn), ",") {
		hasUpgrade = hasUpgrade || strings.TrimSpace(tok) == "upgrade"
	}
	return hasUpgrade && strings.ToLower(up) == "websocket"
}

// Plain 400 for a request the parser refuses, closing the connection
func (h *httpConn) sendInvalid() {
	body := "Invalid HTTP request received."
	var b bytes.Buffer
	b.WriteString("HTTP/1.1 400 Bad Request\r\n")
	for _, hd := range append(h.srv.defaultHeaders(), Header{"content-type", "text/plain; charset=utf-8"},
		Header{"content-length", strconv.Itoa(len(body))}, Header{"connection", "close"}) {
		b.WriteString(hd[0] + ": " + hd[1] + "\r\n")
	}
	b.WriteString("\r\n" + body)
	h.conn.Write(b.Bytes())
}

// One request and response exchange on a connection
type requestCycle struct {
	h              *httpConn
	scope          *Scope
	body           io.Reader
	bodyDone       bool
	expectContinue bool
	keepAlive      bool
	mu             sync.Mutex
	started        bool
	complete       bool
	chunked        bool
	headOnly       bool
	disconnected   bool
	disconnect     chan struct{}
	disconnectOnce sync.Once
}

func newRequestCycle(h *httpConn, scope *Scope, head *requestHead) *requestCycle {
	c := &requestCycle{h: h, scope: scope, keepAlive: true, headOnly: head.method == "HEAD", disconnect: make(chan struct{})}
	connHdr, _ := headValue(head.headers, "connection")
	if strings.EqualFold(strings.TrimSpace(connHdr), "close") || (head.proto == "HTTP/1.0" && !strings.EqualFold(connHdr, "keep-alive")) {
		c.keepAlive = false
	}
	if exp, ok := headValue(head.headers, "expect"); ok && strings.EqualFold(exp, "100-continue") {
		c.expectContinue = true
	}
	if te, ok := headValue(head.headers, "transfer-encoding"); ok && strings.Contains(strings.ToLower(te), "chunked") {
		c.body = httputil.NewChunkedReader(h.br)
	} else if cl, ok := headValue(head.headers, "content-length"); ok {
		n, err := strconv.ParseInt(strings.TrimSpace(cl), 10, 64)
		if err != nil || n < 0 {
			n = 0
		}
		c.body = io.LimitReader(h.br, n)
	} else {
		c.body = bytes.NewReader(nil)
	}
	return c
}

func (c *requestCycle) responseComplete() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.complete
}

// Reads the rest of the request body so the next request starts clean, false when the stream broke
func (c *requestCycle) drainBody() bool {
	_, err := io.Copy(io.Discard, c.body)
	return err == nil
}

// Next body chunk or the disconnect once the response completed or the client left
func (c *requestCycle) receive(ctx context.Context) (Message, error) {
	c.mu.Lock()
	if c.expectContinue && !c.started {
		c.expectContinue = false
		c.mu.Unlock()
		c.h.conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
		c.mu.Lock()
	}
	if c.complete || c.disconnected {
		c.mu.Unlock()
		return Message{Type: "http.disconnect"}, nil
	}
	if c.bodyDone {
		c.mu.Unlock()
		if ctx.Err() != nil {
			return Message{}, ctx.Err()
		}
		select {
		case <-c.disconnect:
			return Message{Type: "http.disconnect"}, nil
		case <-ctx.Done():
			return Message{}, ctx.Err()
		}
	}
	c.mu.Unlock()
	if ctx.Err() != nil {
		return Message{}, ctx.Err()
	}
	buf := make([]byte, 64*1024)
	n, err := c.body.Read(buf)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil && !errors.Is(err, io.EOF) {
		c.disconnected = true
		c.disconnectOnce.Do(func() { close(c.disconnect) })
		return Message{Type: "http.disconnect"}, nil
	}
	more := !errors.Is(err, io.EOF) && !isLimitDone(c.body)
	if !more {
		c.bodyDone = true
	}
	return Message{Type: "http.request", Body: buf[:n], MoreBody: more}, nil
}

// Whether a length limited body has no bytes left
func isLimitDone(r io.Reader) bool {
	lr, ok := r.(*io.LimitedReader)
	return ok && lr.N == 0
}

// Writes the status line and headers or a body chunk, chunked framing when no length was given
func (c *requestCycle) send(ctx context.Context, m Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disconnected {
		return &ClientGone{Err: errors.New("connection closed")}
	}
	switch m.Type {
	case "http.response.start":
		if c.started {
			return &RuntimeError{Msg: "Unexpected ASGI message 'http.response.start' sent, after response already started."}
		}
		c.started = true
		var b bytes.Buffer
		b.WriteString(statusLine(m.Status))
		headers := append(c.h.srv.defaultHeaders(), m.Headers...)
		if !c.keepAlive {
			if _, ok := headValue(headers, "connection"); !ok {
				headers = append(headers, Header{"connection", "close"})
			}
		}
		lengthSet := false
		for _, hd := range headers {
			name := strings.ToLower(hd[0])
			switch {
			case name == "content-length":
				lengthSet = true
			case name == "transfer-encoding" && strings.ToLower(hd[1]) == "chunked":
				lengthSet, c.chunked = true, true
			case name == "connection" && strings.ToLower(hd[1]) == "close":
				c.keepAlive = false
			}
			b.WriteString(name + ": " + hd[1] + "\r\n")
		}
		if !lengthSet && !c.headOnly && m.Status != 204 && m.Status != 304 {
			c.chunked = true
			b.WriteString("transfer-encoding: chunked\r\n")
		}
		b.WriteString("\r\n")
		return c.write(b.Bytes())
	case "http.response.body":
		if !c.started {
			return &RuntimeError{Msg: "Unexpected ASGI message 'http.response.body' sent, before 'http.response.start'."}
		}
		if c.complete {
			return &RuntimeError{Msg: "Unexpected ASGI message 'http.response.body' sent, after response already completed."}
		}
		if c.headOnly {
			if !m.MoreBody {
				c.complete = true
			}
			return nil
		}
		var b bytes.Buffer
		if c.chunked {
			if len(m.Body) > 0 {
				b.WriteString(strconv.FormatInt(int64(len(m.Body)), 16) + "\r\n")
				b.Write(m.Body)
				b.WriteString("\r\n")
			}
			if !m.MoreBody {
				b.WriteString("0\r\n\r\n")
			}
		} else {
			b.Write(m.Body)
		}
		if !m.MoreBody {
			c.complete = true
		}
		return c.write(b.Bytes())
	}
	return &RuntimeError{Msg: "Unexpected ASGI message '" + m.Type + "' sent, after response already completed."}
}

func (c *requestCycle) write(p []byte) error {
	if _, err := c.h.conn.Write(p); err != nil {
		c.disconnected = true
		c.disconnectOnce.Do(func() { close(c.disconnect) })
		return &ClientGone{Err: err}
	}
	return nil
}

// Status line with the Python HTTPStatus phrase, empty for an unknown code
func statusLine(status int) string {
	return "HTTP/1.1 " + strconv.Itoa(status) + " " + statusPhrase(status) + "\r\n"
}

// Runs the app for the request, answering 500 when it fails or returns before starting a response
func (c *requestCycle) run() {
	stop, done := make(chan struct{}), make(chan struct{})
	go c.watchDisconnect(stop, done)
	err := c.h.srv.app(c.h.srv.serveCtx, c.scope, c.receive, c.send)
	close(stop)
	c.h.conn.SetReadDeadline(time.Now())
	<-done
	c.h.conn.SetReadDeadline(time.Time{})
	c.mu.Lock()
	started, complete := c.started, c.complete
	c.mu.Unlock()
	if err != nil {
		uvicornLog().Error("Exception in ASGI application\n", "error", err)
		if !started {
			c.send500()
		} else {
			c.keepAlive = false
		}
		return
	}
	if !started {
		uvicornLog().Error("ASGI callable returned without starting response.")
		c.send500()
		return
	}
	if !complete {
		uvicornLog().Error("ASGI callable returned without completing response.")
		c.keepAlive = false
	}
}

// Notices the client leaving once the whole request body was read, until the app returns
func (c *requestCycle) watchDisconnect(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	for {
		c.mu.Lock()
		bodyDone := c.bodyDone
		c.mu.Unlock()
		if bodyDone {
			break
		}
		select {
		case <-stop:
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
	for {
		_, err := c.h.br.Peek(1)
		if err == nil {
			return
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			select {
			case <-stop:
				return
			default:
				continue
			}
		}
		c.mu.Lock()
		c.disconnected = true
		c.disconnectOnce.Do(func() { close(c.disconnect) })
		c.mu.Unlock()
		return
	}
}

// Plain 500 response closing the connection
func (c *requestCycle) send500() {
	c.keepAlive = false
	c.send(context.Background(), Message{Type: "http.response.start", Status: 500, Headers: []Header{
		{"content-type", "text/plain; charset=utf-8"}, {"content-length", "21"}, {"connection", "close"}}})
	c.send(context.Background(), Message{Type: "http.response.body", Body: []byte("Internal Server Error")})
}
