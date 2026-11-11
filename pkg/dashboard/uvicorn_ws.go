package dashboard

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Response writer over a raw connection that only hijacks, the shape the gorilla upgrader needs
type hijackWriter struct {
	conn net.Conn
	br   *bufio.Reader
	hdr  http.Header
}

func (w *hijackWriter) Header() http.Header         { return w.hdr }
func (w *hijackWriter) Write(p []byte) (int, error) { return w.conn.Write(p) }
func (w *hijackWriter) WriteHeader(int)             {}
func (w *hijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(w.br, bufio.NewWriter(w.conn)), nil
}

// Websocket connection of one upgraded request, the ASGI websocket protocol over gorilla
type wsSession struct {
	h          *httpConn
	scope      *Scope
	head       *requestHead
	mu         sync.Mutex
	conn       *websocket.Conn
	connected  bool
	handshook  bool
	closed     bool
	closing    bool
	sentFirst  bool
	denial     bool
	denialBody bytes.Buffer
	denialHead Message
	pongAt     time.Time
	readMsgs   chan Message
}

// Serves the websocket app for an upgrade request until it ends
func serveWebSocket(h *httpConn, scope *Scope, head *requestHead) {
	if p, ok := headValue(head.headers, "sec-websocket-protocol"); ok {
		for _, v := range strings.Split(p, ",") {
			scope.Subprotocols = append(scope.Subprotocols, strings.TrimSpace(v))
		}
	}
	scope.Extensions["websocket.http.response"] = map[string]any{}
	ws := &wsSession{h: h, scope: scope, head: head, readMsgs: make(chan Message, 16)}
	h.mu.Lock()
	h.ws = ws
	h.mu.Unlock()
	err := h.srv.app(h.srv.serveCtx, scope, ws.receive, ws.send)
	ws.mu.Lock()
	handshook, closed, denial := ws.handshook, ws.closed, ws.denial
	ws.mu.Unlock()
	if err != nil {
		uvicornLog().Error("Exception in ASGI application\n", "error", err)
		if !handshook && !denial {
			ws.sendHTTP(500, nil, []byte("Internal Server Error"))
		} else if handshook && !closed {
			ws.closeWith(1011, "")
		}
		return
	}
	if !handshook && !denial && !closed {
		uvicornLog().Error("ASGI callable returned without sending handshake.")
		ws.sendHTTP(500, nil, []byte("Internal Server Error"))
		return
	}
	if handshook && !closed {
		uvicornLog().Error("ASGI callable returned without closing connection.")
	}
}

// Writes a plain HTTP response in place of the handshake
func (ws *wsSession) sendHTTP(status int, headers []Header, body []byte) {
	var b bytes.Buffer
	b.WriteString(statusLine(status))
	all := append(ws.h.srv.defaultHeaders(), headers...)
	if _, ok := headValue(all, "content-length"); !ok {
		all = append(all, Header{"content-length", strconv.Itoa(len(body))})
	}
	if _, ok := headValue(all, "content-type"); !ok && status == 500 {
		all = append(all, Header{"content-type", "text/plain; charset=utf-8"})
	}
	all = append(all, Header{"connection", "close"})
	for _, hd := range all {
		b.WriteString(strings.ToLower(hd[0]) + ": " + hd[1] + "\r\n")
	}
	b.WriteString("\r\n")
	b.Write(body)
	ws.h.conn.Write(b.Bytes())
}

// Connect first, then frames from the peer, then the disconnect with its close code
func (ws *wsSession) receive(ctx context.Context) (Message, error) {
	ws.mu.Lock()
	if !ws.sentFirst {
		ws.sentFirst = true
		ws.mu.Unlock()
		return Message{Type: "websocket.connect"}, nil
	}
	handshook := ws.handshook
	ws.mu.Unlock()
	if !handshook {
		<-ctx.Done()
		return Message{}, ctx.Err()
	}
	select {
	case m, ok := <-ws.readMsgs:
		if !ok {
			return Message{Type: "websocket.disconnect", Code: 1006}, nil
		}
		return m, nil
	case <-ctx.Done():
		return Message{}, ctx.Err()
	}
}

// Accepts, refuses, denies with a response, sends frames or closes per the app event
func (ws *wsSession) send(ctx context.Context, m Message) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if !ws.handshook && !ws.closed {
		switch m.Type {
		case "websocket.accept":
			return ws.accept(m)
		case "websocket.close":
			ws.closed = true
			ws.sendHTTP(403, nil, nil)
			return nil
		case "websocket.http.response.start":
			ws.denial, ws.denialHead = true, m
			return nil
		case "websocket.http.response.body":
			if !ws.denial {
				break
			}
			ws.denialBody.Write(m.Body)
			if !m.MoreBody {
				ws.closed = true
				ws.sendHTTP(ws.denialHead.Status, ws.denialHead.Headers, ws.denialBody.Bytes())
			}
			return nil
		}
		return &RuntimeError{Msg: "Expected ASGI message 'websocket.accept', 'websocket.close', or 'websocket.http.response.start' but got '" + m.Type + "'."}
	}
	if ws.closed {
		return &ClientGone{Err: errors.New("Unexpected ASGI message '" + m.Type + "', after sending 'websocket.close' or response already completed.")}
	}
	switch m.Type {
	case "websocket.send":
		var err error
		if m.Text != nil {
			err = ws.conn.WriteMessage(websocket.TextMessage, []byte(*m.Text))
		} else {
			err = ws.conn.WriteMessage(websocket.BinaryMessage, m.Bytes)
		}
		if err != nil {
			return &ClientGone{Err: err}
		}
		return nil
	case "websocket.close":
		ws.closed = true
		code := m.Code
		if code == 0 {
			code = 1000
		}
		ws.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, m.Reason), time.Now().Add(5*time.Second))
		return nil
	}
	return &RuntimeError{Msg: "Expected ASGI message 'websocket.send' or 'websocket.close', but got '" + m.Type + "'."}
}

// Completes the handshake with the chosen subprotocol and extra headers, then starts the reader and pinger
func (ws *wsSession) accept(m Message) error {
	cfg := ws.h.srv.cfg
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }, EnableCompression: cfg.WSPerMessageDeflate}
	respHeader := http.Header{}
	for _, hd := range append(ws.h.srv.defaultHeaders(), m.Headers...) {
		respHeader[strings.ToLower(hd[0])] = append(respHeader[strings.ToLower(hd[0])], hd[1])
	}
	if m.Subprotocol != nil {
		respHeader["Sec-Websocket-Protocol"] = []string{*m.Subprotocol}
	}
	req := &http.Request{Method: "GET", Header: http.Header{}, ProtoMajor: 1, ProtoMinor: 1}
	for _, hd := range ws.head.headers {
		req.Header.Add(hd[0], hd[1])
	}
	conn, err := up.Upgrade(&hijackWriter{conn: ws.h.conn, br: ws.h.br, hdr: http.Header{}}, req, respHeader)
	if err != nil {
		ws.closed = true
		return &ClientGone{Err: err}
	}
	ws.conn, ws.handshook = conn, true
	if cfg.WSMaxSize > 0 {
		conn.SetReadLimit(cfg.WSMaxSize)
	}
	ws.pongAt = time.Now()
	conn.SetPongHandler(func(string) error {
		ws.mu.Lock()
		ws.pongAt = time.Now()
		ws.mu.Unlock()
		return nil
	})
	go ws.readLoop()
	if cfg.WSPingInterval != nil && *cfg.WSPingInterval > 0 {
		go ws.pingLoop(*cfg.WSPingInterval, cfg.WSPingTimeout)
	}
	return nil
}

// Turns peer frames into receive events and the end of the stream into a disconnect
func (ws *wsSession) readLoop() {
	for {
		typ, data, err := ws.conn.ReadMessage()
		if err != nil {
			code, reason := 1006, ""
			var ce *websocket.CloseError
			switch {
			case errors.As(err, &ce):
				code, reason = ce.Code, ce.Text
				if code == websocket.CloseNoStatusReceived {
					code = 1005
				}
			case errors.Is(err, websocket.ErrReadLimit):
				code = 1009
			}
			ws.mu.Lock()
			if ws.closing {
				code = 1012
			}
			ws.closed = true
			ws.mu.Unlock()
			ws.readMsgs <- Message{Type: "websocket.disconnect", Code: code, Reason: reason}
			ws.conn.Close()
			return
		}
		if typ == websocket.TextMessage {
			text := string(data)
			ws.readMsgs <- Message{Type: "websocket.receive", Text: &text}
		} else {
			ws.readMsgs <- Message{Type: "websocket.receive", Bytes: data}
		}
	}
}

// Pings every interval and drops the connection when no pong arrives within the timeout
func (ws *wsSession) pingLoop(interval time.Duration, timeout *time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		ws.mu.Lock()
		closed := ws.closed
		ws.mu.Unlock()
		if closed {
			return
		}
		sent := time.Now()
		if err := ws.conn.WriteControl(websocket.PingMessage, nil, sent.Add(interval)); err != nil {
			return
		}
		if timeout == nil || *timeout <= 0 {
			continue
		}
		time.Sleep(*timeout)
		ws.mu.Lock()
		late := ws.pongAt.Before(sent)
		ws.mu.Unlock()
		if late {
			ws.conn.Close()
			return
		}
	}
}

// Sends a close frame with the code and drops the connection
func (ws *wsSession) closeWith(code int, reason string) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.conn == nil || ws.closed {
		return
	}
	ws.closed = true
	ws.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	ws.conn.Close()
}

// Server shutdown of the socket, a 1012 close after the handshake and a 500 before it
func (ws *wsSession) shutdown() {
	ws.mu.Lock()
	ws.closing = true
	handshook := ws.handshook
	ws.mu.Unlock()
	if handshook {
		ws.closeWith(1012, "")
		return
	}
	ws.sendHTTP(500, nil, []byte("Internal Server Error"))
	ws.h.conn.Close()
}
