package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Socket of one admitted session as the session handler drives it
type SessionSocket interface {
	SendText(ctx context.Context, text string) error
	Close(code int) error
	Accept(ctx context.Context, subprotocol *string) error
	ReceiveText(ctx context.Context) (string, error)
	Peer() (host string, port int, known bool)
	RawConn() net.Conn
	SessionAuthority() *gateway.SessionAuthority
}

// Peer close a session socket read reports, with its close code and reason
type SessionSocketClosed struct {
	Code   int
	Reason string
}

func (e *SessionSocketClosed) Error() string {
	return fmt.Sprintf("session socket closed (%d, %q)", e.Code, e.Reason)
}

// Session handler an admitted socket runs, assigned by the command wiring
var ServeSessionSocket func(ctx context.Context, sock SessionSocket, identity map[string]any, subprotocol *string, operator bool) error

// Largest frame a session socket reads
const sessionSocketMaxBytes = 384 * 1024 * 1024

// One frame the reader hands the session, the peer close when gone is set
type socketFrame struct {
	text   string
	binary bool
	gone   bool
	code   int
	reason string
}

// Session socket of one upgrade request, the surface the session handler serves
type sessionSocket struct {
	listener  *sessionListener
	w         http.ResponseWriter
	req       *http.Request
	authority *gateway.SessionAuthority
	// True once the gateway stopped admitting this socket, nil before admission
	shut   func() bool
	frames chan socketFrame
	done   chan struct{}
	// Serializes data frames, control frames go around it
	writeMu  sync.Mutex
	mu       sync.Mutex
	conn     *websocket.Conn
	answered bool
	accepted bool
	closed   bool
	left     bool
	stopping bool
}

// Request header the boundary checks read, the Host line included
func (s *sessionSocket) BoundaryHeader(name string) string {
	if strings.EqualFold(name, "host") {
		return s.req.Host
	}
	return s.req.Header.Get(name)
}

// Peer host of the request, false when the server reports none
func (s *sessionSocket) BoundaryClient() (string, bool) {
	host, _, err := net.SplitHostPort(s.req.RemoteAddr)
	return host, err == nil && host != ""
}

// Peer host and port of the request, not known when the server reports none
func (s *sessionSocket) Peer() (string, int, bool) {
	host, rawPort, err := net.SplitHostPort(s.req.RemoteAddr)
	if err != nil {
		return "", 0, false
	}
	port, err := strconv.Atoi(rawPort)
	return host, port, err == nil
}

// Peer as the refusal log names it, the host and port tuple or None
func (s *sessionSocket) peerText() string {
	host, port, known := s.Peer()
	if !known {
		return "None"
	}
	return fmt.Sprintf("(%s, %d)", pyrepr.Repr(host), port)
}

func (s *sessionSocket) SessionAuthority() *gateway.SessionAuthority { return s.authority }

func (s *sessionSocket) wasAccepted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepted
}

func (s *sessionSocket) wasAnswered() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.answered
}

// Answers the upgrade with 403 unless it was answered already
func (s *sessionSocket) refuse() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.answered {
		return
	}
	s.answered, s.closed = true, true
	s.w.Header().Set("Connection", "close")
	s.w.WriteHeader(http.StatusForbidden)
}

// Completes the handshake with the subprotocol and starts the frame reader
func (s *sessionSocket) Accept(_ context.Context, subprotocol *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.answered {
		return errors.New("session socket was answered before its handshake")
	}
	if s.shut != nil && s.shut() {
		return errors.New("gateway stopped admitting the session socket before its handshake")
	}
	header := http.Header{}
	if subprotocol != nil && *subprotocol != "" {
		header.Set("Sec-Websocket-Protocol", *subprotocol)
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(s.w, s.req, header)
	s.answered = true
	if err != nil {
		s.closed = true
		return err
	}
	conn.SetReadLimit(sessionSocketMaxBytes)
	s.conn, s.accepted = conn, true
	if !s.listener.track(s) {
		s.closed, s.stopping = true, true
		conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseServiceRestart, ""), time.Now().Add(time.Second))
		conn.Close()
		return errors.New("gateway listener stopped during the session socket handshake")
	}
	go s.readLoop(conn)
	return nil
}

// Turns peer frames into session frames and the end of the stream into the peer close
func (s *sessionSocket) readLoop(conn *websocket.Conn) {
	for {
		kind, data, err := conn.ReadMessage()
		frame := socketFrame{text: string(data), binary: kind != websocket.TextMessage}
		if err != nil {
			frame = socketFrame{gone: true, code: websocket.CloseAbnormalClosure}
			var closed *websocket.CloseError
			switch {
			case errors.As(err, &closed):
				frame.code, frame.reason = closed.Code, closed.Text
			case errors.Is(err, websocket.ErrReadLimit):
				frame.code = websocket.CloseMessageTooBig
			}
			s.mu.Lock()
			if s.stopping {
				frame.code = websocket.CloseServiceRestart
			}
			s.closed = true
			s.mu.Unlock()
		}
		select {
		case s.frames <- frame:
		case <-s.done:
			conn.Close()
			return
		}
		if frame.gone {
			conn.Close()
			return
		}
	}
}

// Next text frame, the peer close once it left or the gateway stopped admitting the socket
func (s *sessionSocket) ReceiveText(ctx context.Context) (string, error) {
	s.mu.Lock()
	accepted, left := s.accepted, s.left
	s.mu.Unlock()
	if !accepted {
		return "", errors.New("session socket read before its handshake")
	}
	if left {
		return "", errors.New("session socket read after its disconnect")
	}
	var frame socketFrame
	select {
	case frame = <-s.frames:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if s.shut != nil && s.shut() {
		frame = socketFrame{gone: true, code: websocket.CloseTryAgainLater}
	}
	if frame.gone {
		s.mu.Lock()
		s.left = true
		s.mu.Unlock()
		return "", &SessionSocketClosed{Code: frame.code, Reason: frame.reason}
	}
	if frame.binary {
		return "", errors.New("session socket received a binary frame")
	}
	return frame.text, nil
}

// Writes one text frame, an error once the socket closed
func (s *sessionSocket) SendText(_ context.Context, text string) error {
	s.mu.Lock()
	conn, open := s.conn, s.accepted && !s.closed
	s.mu.Unlock()
	if !open {
		return errors.New("session socket is not open")
	}
	s.writeMu.Lock()
	err := conn.WriteMessage(websocket.TextMessage, []byte(text))
	s.writeMu.Unlock()
	if err != nil {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
	}
	return err
}

// Sends the close frame with the code, a 403 answer when the handshake never completed
func (s *sessionSocket) Close(code int) error {
	s.mu.Lock()
	if !s.answered {
		s.mu.Unlock()
		s.refuse()
		return nil
	}
	if !s.accepted || s.closed {
		s.mu.Unlock()
		return errors.New("session socket is already closed")
	}
	s.closed = true
	conn := s.conn
	s.mu.Unlock()
	if code == 0 {
		code = websocket.CloseNormalClosure
	}
	conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""), time.Now().Add(listenerGrace))
	return nil
}

// Stream socket under the connection, nil before the handshake
func (s *sessionSocket) RawConn() net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return nil
	}
	return s.conn.NetConn()
}

// Ends an open socket with the service restart close as the listener stops
func (s *sessionSocket) goingAway() {
	s.mu.Lock()
	s.stopping = true
	conn, open := s.conn, s.accepted && !s.closed
	if open {
		s.closed = true
	}
	s.mu.Unlock()
	if open {
		conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseServiceRestart, ""), time.Now().Add(time.Second))
		conn.Close()
	}
}

// Drops the connection of a socket that outlived the stop grace
func (s *sessionSocket) drop() {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
}

// Ends the request once its handler returned, 500 with no answer and an internal error close after a failure
func (s *sessionSocket) finish(failure error) {
	close(s.done)
	s.listener.untrack(s)
	s.mu.Lock()
	answered, conn, open := s.answered, s.conn, s.accepted && !s.closed
	s.answered = true
	if open {
		s.closed = true
	}
	s.mu.Unlock()
	switch {
	case !answered:
		http.Error(s.w, "Internal Server Error", http.StatusInternalServerError)
	case open && failure != nil:
		conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, ""), time.Now().Add(time.Second))
		conn.Close()
	case open:
		conn.Close()
	case conn != nil:
		conn.SetReadDeadline(time.Now().Add(listenerGrace))
	}
}
