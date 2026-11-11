package dashboard

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Lifecycle state of one side of a websocket
type WebSocketState int

const (
	WSConnecting WebSocketState = iota
	WSConnected
	WSDisconnected
	WSResponse
)

// Peer closed the socket, carrying its close code and reason
type WebSocketDisconnect struct {
	Code   int
	Reason string
}

func (e *WebSocketDisconnect) Error() string      { return fmt.Sprintf("(%d, %q)", e.Code, e.Reason) }
func (e *WebSocketDisconnect) PyTypeName() string { return "WebSocketDisconnect" }

// Use of a socket after its disconnect or close, a Python RuntimeError subclass
type WebSocketDisconnected struct{ Msg string }

func (e *WebSocketDisconnected) Error() string      { return e.Msg }
func (e *WebSocketDisconnected) PyTypeName() string { return "WebSocketDisconnected" }

// Server side websocket over the ASGI events with the Starlette state checks
type WebSocket struct {
	HTTPConnection
	mu               sync.Mutex
	ClientState      WebSocketState
	ApplicationState WebSocketState
}

// Websocket over a websocket scope and its receive and send callables
func NewWebSocket(scope *Scope, receive Receive, send Send) *WebSocket {
	return &WebSocket{HTTPConnection: HTTPConnection{Scope: scope, receive: receive, send: send}}
}

// Next event with the connect, receive and disconnect order enforced
func (w *WebSocket) Receive(ctx context.Context) (Message, error) {
	w.mu.Lock()
	state := w.ClientState
	w.mu.Unlock()
	switch state {
	case WSConnecting:
		m, err := w.receive(ctx)
		if err != nil {
			return m, err
		}
		if m.Type != "websocket.connect" {
			return m, &RuntimeError{Msg: fmt.Sprintf("Expected ASGI message \"websocket.connect\", but got '%s'", m.Type)}
		}
		w.setClient(WSConnected)
		return m, nil
	case WSConnected:
		m, err := w.receive(ctx)
		if err != nil {
			return m, err
		}
		if m.Type != "websocket.receive" && m.Type != "websocket.disconnect" {
			return m, &RuntimeError{Msg: fmt.Sprintf("Expected ASGI message \"websocket.receive\" or \"websocket.disconnect\", but got '%s'", m.Type)}
		}
		if m.Type == "websocket.disconnect" {
			w.setClient(WSDisconnected)
		}
		return m, nil
	}
	return Message{}, &WebSocketDisconnected{Msg: "Cannot call \"receive\" once a disconnect message has been received."}
}

func (w *WebSocket) setClient(s WebSocketState) {
	w.mu.Lock()
	w.ClientState = s
	w.mu.Unlock()
}

func (w *WebSocket) setApp(s WebSocketState) {
	w.mu.Lock()
	w.ApplicationState = s
	w.mu.Unlock()
}

// State of the application side
func (w *WebSocket) AppState() WebSocketState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ApplicationState
}

// State of the client side
func (w *WebSocket) PeerState() WebSocketState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ClientState
}

// Sends one event with the accept, send, close and denial order enforced
func (w *WebSocket) Send(ctx context.Context, m Message) error {
	switch w.AppState() {
	case WSConnecting:
		if m.Type != "websocket.accept" && m.Type != "websocket.close" && m.Type != "websocket.http.response.start" {
			return &RuntimeError{Msg: fmt.Sprintf("Expected ASGI message \"websocket.accept\", \"websocket.close\" or \"websocket.http.response.start\", but got '%s'", m.Type)}
		}
		switch m.Type {
		case "websocket.close":
			w.setApp(WSDisconnected)
		case "websocket.http.response.start":
			w.setApp(WSResponse)
		default:
			w.setApp(WSConnected)
		}
		return w.send(ctx, m)
	case WSConnected:
		if m.Type != "websocket.send" && m.Type != "websocket.close" {
			return &RuntimeError{Msg: fmt.Sprintf("Expected ASGI message \"websocket.send\" or \"websocket.close\", but got '%s'", m.Type)}
		}
		if m.Type == "websocket.close" {
			w.setApp(WSDisconnected)
		}
		if err := w.send(ctx, m); err != nil {
			var oe *ClientGone
			if errors.As(err, &oe) {
				w.setApp(WSDisconnected)
				return &WebSocketDisconnect{Code: 1006}
			}
			return err
		}
		return nil
	case WSResponse:
		if m.Type != "websocket.http.response.body" {
			return &RuntimeError{Msg: fmt.Sprintf("Expected ASGI message \"websocket.http.response.body\", but got '%s'", m.Type)}
		}
		if !m.MoreBody {
			w.setApp(WSDisconnected)
		}
		return w.send(ctx, m)
	}
	return &WebSocketDisconnected{Msg: "Cannot call \"send\" once a close message has been sent."}
}

// Write to a peer that is gone, the OSError a server send raises
type ClientGone struct{ Err error }

func (e *ClientGone) Error() string      { return e.Err.Error() }
func (e *ClientGone) Unwrap() error      { return e.Err }
func (e *ClientGone) PyTypeName() string { return "OSError" }

// Accepts the handshake with an optional subprotocol and extra headers, waiting for the connect event first
func (w *WebSocket) Accept(ctx context.Context, subprotocol *string, headers []Header) error {
	if headers == nil {
		headers = []Header{}
	}
	if w.PeerState() == WSConnecting {
		if _, err := w.Receive(ctx); err != nil {
			return err
		}
	}
	return w.Send(ctx, Message{Type: "websocket.accept", Subprotocol: subprotocol, Headers: headers})
}

func (w *WebSocket) requireConnected() error {
	if w.AppState() != WSConnected {
		return &WebSocketDisconnected{Msg: "WebSocket is not connected. Need to call \"accept\" first."}
	}
	return nil
}

func raiseOnDisconnect(m Message) error {
	if m.Type == "websocket.disconnect" {
		return &WebSocketDisconnect{Code: m.Code, Reason: m.Reason}
	}
	return nil
}

// Next text frame, the disconnect error once the peer leaves
func (w *WebSocket) ReceiveText(ctx context.Context) (string, error) {
	if err := w.requireConnected(); err != nil {
		return "", err
	}
	m, err := w.Receive(ctx)
	if err != nil {
		return "", err
	}
	if err := raiseOnDisconnect(m); err != nil {
		return "", err
	}
	if m.Text == nil {
		return "", &KeyError{Key: "text"}
	}
	return *m.Text, nil
}

// Next binary frame, the disconnect error once the peer leaves
func (w *WebSocket) ReceiveBytes(ctx context.Context) ([]byte, error) {
	if err := w.requireConnected(); err != nil {
		return nil, err
	}
	m, err := w.Receive(ctx)
	if err != nil {
		return nil, err
	}
	if err := raiseOnDisconnect(m); err != nil {
		return nil, err
	}
	if m.Bytes == nil {
		return nil, &KeyError{Key: "bytes"}
	}
	return m.Bytes, nil
}

// Next frame decoded as JSON from its text or, in binary mode, its utf-8 bytes
func (w *WebSocket) ReceiveJSON(ctx context.Context, binary bool) (any, error) {
	if err := w.requireConnected(); err != nil {
		return nil, err
	}
	m, err := w.Receive(ctx)
	if err != nil {
		return nil, err
	}
	if err := raiseOnDisconnect(m); err != nil {
		return nil, err
	}
	var text string
	if !binary {
		if m.Text == nil {
			return nil, &KeyError{Key: "text"}
		}
		text = *m.Text
	} else {
		if m.Bytes == nil {
			return nil, &KeyError{Key: "bytes"}
		}
		if !utf8.Valid(m.Bytes) {
			return nil, pystr.UTF8DecodeError(m.Bytes, 0)
		}
		text = string(m.Bytes)
	}
	return pyjson.Loads(text)
}

// Sends a text frame
func (w *WebSocket) SendText(ctx context.Context, data string) error {
	return w.Send(ctx, Message{Type: "websocket.send", Text: &data})
}

// Sends a binary frame
func (w *WebSocket) SendBytes(ctx context.Context, data []byte) error {
	return w.Send(ctx, Message{Type: "websocket.send", Bytes: data})
}

// Sends compact JSON as a text frame or, in binary mode, its utf-8 bytes
func (w *WebSocket) SendJSON(ctx context.Context, data any, binary bool) error {
	text, err := pyjson.Dumps(data, pyjson.Separators(",", ":"), pyjson.EnsureASCII(false))
	if err != nil {
		return err
	}
	if binary {
		return w.SendBytes(ctx, []byte(text))
	}
	return w.SendText(ctx, text)
}

// Sends the close event with a code and reason
func (w *WebSocket) Close(ctx context.Context, code int, reason string) error {
	return w.Send(ctx, Message{Type: "websocket.close", Code: code, Reason: reason})
}

// Sends an HTTP response in place of the handshake when the server supports the denial extension
func (w *WebSocket) SendDenialResponse(ctx context.Context, resp ASGIResponse) error {
	if _, ok := w.Scope.Extensions["websocket.http.response"]; ok {
		return resp.Serve(ctx, w.Scope, w.Receive, w.Send)
	}
	return &RuntimeError{Msg: "The server doesn't support the Websocket Denial Response extension."}
}

// Python KeyError of a missing message field
type KeyError struct{ Key string }

func (e *KeyError) Error() string      { return "'" + e.Key + "'" }
func (e *KeyError) PyTypeName() string { return "KeyError" }

// Close event the router sends for a websocket no route matches
func websocketClose(code int, reason string) ASGIApp {
	return func(ctx context.Context, scope *Scope, receive Receive, send Send) error {
		return send(ctx, Message{Type: "websocket.close", Code: code, Reason: reason})
	}
}
