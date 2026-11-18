package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Frame socket of a gateway link, a WebSocket in production
type wireSocket interface {
	Recv() (raw string, err error)
	Send(data string) error
	Open() bool
	Subprotocol() string
	Close() error
}

// Socket over a gorilla WebSocket connection with its open state tracked
type wsSocket struct {
	conn   *websocket.Conn
	mu     sync.Mutex
	closed bool
}

func (s *wsSocket) Recv() (string, error) {
	_, data, err := s.conn.ReadMessage()
	if err != nil {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		return "", err
	}
	return string(data), nil
}

func (s *wsSocket) Send(data string) error {
	return s.conn.WriteMessage(websocket.TextMessage, []byte(data))
}

func (s *wsSocket) Open() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed
}

func (s *wsSocket) Subprotocol() string { return s.conn.Subprotocol() }

func (s *wsSocket) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return s.conn.Close()
}

// Socket on a gateway URL with no frame cap and no proxy, refused unless the server chose the first protocol
func dialGatewaySocket(ctx context.Context, target string, protocols []string) (wireSocket, error) {
	dialer := websocket.Dialer{Proxy: nil, HandshakeTimeout: 10 * time.Second, Subprotocols: protocols}
	conn, _, err := dialer.DialContext(ctx, target, http.Header{})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(0)
	sock := &wsSocket{conn: conn}
	if len(protocols) > 0 && sock.Subprotocol() != protocols[0] {
		sock.Close()
		return nil, errors.New("gateway protocol mismatch, update or restart required")
	}
	return sock, nil
}

// Refusal frame of a request, the error member as the gateway sent it
type wireRefusal struct{ member any }

func (e *wireRefusal) Error() string { return "gateway refused the request" }

// Request the socket could not carry, sent false when it never left this process
type linkGoneError struct{ sent bool }

func (e *linkGoneError) Error() string {
	if e.sent {
		return "gateway disconnected before it answered"
	}
	return "gateway disconnected before the request was sent"
}

type wireAnswer struct {
	result any
	err    error
}

// JSON-RPC link over one socket, answers matched to their requests and every other frame queued in order
type wireLink struct {
	socket    wireSocket
	inbound   func(frame map[string]any)
	malformed func(raw string)

	mu       sync.Mutex
	writeMu  sync.Mutex
	pending  map[int64]chan wireAnswer
	seq      int64
	queue    []map[string]any
	moved    chan struct{}
	ended    bool
	readErr  error
	readDone chan struct{}
}

// Link over a socket, inbound seeing every object frame and malformed every frame that is none
func newWireLink(socket wireSocket, inbound func(map[string]any), malformed func(string)) *wireLink {
	return &wireLink{socket: socket, inbound: inbound, malformed: malformed, pending: map[int64]chan wireAnswer{},
		moved: make(chan struct{}), readDone: make(chan struct{})}
}

// Starts the reader
func (l *wireLink) start() *wireLink {
	go l.read()
	return l
}

// Closes the socket and waits for the reader to fail every pending answer
func (l *wireLink) close() error {
	err := l.socket.Close()
	<-l.readDone
	return err
}

// Error the socket read ended with, nil while the reader runs
func (l *wireLink) readError() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.readErr
}

// Next queued frame, false once the reader ended and the queue is empty
func (l *wireLink) next() (map[string]any, bool) {
	for {
		l.mu.Lock()
		if len(l.queue) > 0 {
			frame := l.queue[0]
			l.queue = l.queue[1:]
			l.mu.Unlock()
			return frame, true
		}
		if l.ended {
			l.mu.Unlock()
			return nil, false
		}
		moved := l.moved
		l.mu.Unlock()
		<-moved
	}
}

// Wakes a waiting consumer, under the link lock
func (l *wireLink) wakeLocked() {
	close(l.moved)
	l.moved = make(chan struct{})
}

// Reads frames until the socket ends, answering requests and queueing the rest, then fails every waiter
func (l *wireLink) read() {
	defer close(l.readDone)
	defer func() {
		l.mu.Lock()
		for id, waiter := range l.pending {
			waiter <- wireAnswer{err: &linkGoneError{sent: true}}
			delete(l.pending, id)
		}
		l.ended = true
		l.wakeLocked()
		l.mu.Unlock()
	}()
	for {
		raw, err := l.socket.Recv()
		if err != nil {
			l.mu.Lock()
			l.readErr = err
			l.mu.Unlock()
			return
		}
		var frame map[string]any
		if json.Unmarshal([]byte(raw), &frame) != nil || frame == nil {
			l.malformed(raw)
			continue
		}
		l.inbound(frame)
		l.mu.Lock()
		var waiter chan wireAnswer
		if id, isNum := frame["id"].(float64); isNum && id == float64(int64(id)) {
			waiter = l.pending[int64(id)]
			delete(l.pending, int64(id))
		}
		_, isMethod := frame["method"]
		switch {
		case waiter != nil:
			if member, refused := frame["error"]; refused {
				waiter <- wireAnswer{err: &wireRefusal{member: member}}
			} else if result, answered := frame["result"]; answered {
				waiter <- wireAnswer{result: result}
			} else {
				waiter <- wireAnswer{result: map[string]any{}}
			}
		case isMethod:
			l.queue = append(l.queue, frame)
			l.wakeLocked()
		}
		l.mu.Unlock()
	}
}

// Writes one raw frame on the socket
func (l *wireLink) sendFrame(text string) error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	return l.socket.Send(text)
}

// Frame text of a value, non ASCII and markup characters kept as they are
func encodeFrame(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// One request and its answer, waiting until the context ends
func (l *wireLink) call(ctx context.Context, method string, params map[string]any) (any, error) {
	if !l.socket.Open() {
		return nil, &linkGoneError{}
	}
	if params == nil {
		params = map[string]any{}
	}
	answer := make(chan wireAnswer, 1)
	l.mu.Lock()
	if l.ended {
		l.mu.Unlock()
		return nil, &linkGoneError{}
	}
	l.seq++
	id := l.seq
	l.pending[id] = answer
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.pending, id)
		l.mu.Unlock()
	}()
	text, err := encodeFrame(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if err := l.sendFrame(text); err != nil {
		return nil, &linkGoneError{sent: true}
	}
	select {
	case a := <-answer:
		return a.result, a.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
