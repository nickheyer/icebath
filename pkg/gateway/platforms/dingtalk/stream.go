package dingtalk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Stream gateway endpoint trading app credentials for a websocket endpoint and ticket
var openConnectionURL = "https://api.dingtalk.com/v1.0/gateway/connections/open"

const (
	frameTypeSystem       = "SYSTEM"
	frameTypeEvent        = "EVENT"
	frameTypeCallback     = "CALLBACK"
	systemTopicPing       = "ping"
	systemTopicDisconnect = "disconnect"

	// AckOK acknowledges a frame the handler processed
	AckOK = 200
	// AckNotImplemented acknowledges a frame no handler is registered for
	AckNotImplemented = 404
	// AckSystemException acknowledges a frame the handler failed on
	AckSystemException = 500

	keepaliveInterval = 60 * time.Second
	readIdleTimeout   = 150 * time.Second
	writeTimeout      = 10 * time.Second
	maxFrameBytes     = 16 << 20
)

// Frame is one stream protocol message with its type, headers and JSON data text
type Frame struct {
	SpecVersion string         `json:"specVersion"`
	Type        string         `json:"type"`
	Headers     map[string]any `json:"headers"`
	Data        string         `json:"data"`
}

// Header value of the frame as text
func (f *Frame) Header(key string) string { return str(f.Headers[key]) }

// Topic the frame was published on
func (f *Frame) Topic() string { return f.Header("topic") }

// Message id the acknowledgement echoes
func (f *Frame) MessageID() string { return f.Header("messageId") }

// FrameHandler handles one callback or event frame and returns the ack code and message
type FrameHandler func(ctx context.Context, frame *Frame) (code int, message string)

// Acknowledgement written back for a handled frame
type ackFrame struct {
	Code    int               `json:"code"`
	Headers map[string]string `json:"headers"`
	Message string            `json:"message"`
	Data    string            `json:"data"`
}

// Failure of one stream session with the kind the reconnect breaker compares
type streamError struct {
	kind   string
	status int
	err    error
}

func (e *streamError) Error() string { return e.err.Error() }

func (e *streamError) Unwrap() error { return e.err }

// Whether the stream gateway refused the app credentials
func (e *streamError) credentialsRejected() bool {
	return e.kind == "open" && (e.status == http.StatusBadRequest || e.status == http.StatusUnauthorized || e.status == http.StatusForbidden)
}

// One registered callback route and whether it runs on the read loop to keep arrival order
type callbackRoute struct {
	handler FrameHandler
	inline  bool
}

// StreamClient holds the stream mode connection of one DingTalk app and routes its frames to handlers
type StreamClient struct {
	clientID     string
	clientSecret string
	http         *http.Client
	dialer       *websocket.Dialer
	localIP      string

	mu          sync.Mutex
	callbacks   map[string]callbackRoute
	events      FrameHandler
	conn        *websocket.Conn
	resubscribe bool

	writeMu sync.Mutex
}

// Client for the app credentials over the given HTTP client and websocket dialer
func newStreamClient(clientID, clientSecret string, httpClient *http.Client, dialer *websocket.Dialer) *StreamClient {
	return &StreamClient{clientID: clientID, clientSecret: clientSecret, http: httpClient, dialer: dialer, localIP: lanIP(),
		callbacks: map[string]callbackRoute{}}
}

// RegisterCallbackHandler routes callback frames of the topic to the handler, reconnecting a live stream to subscribe it
func (s *StreamClient) RegisterCallbackHandler(topic string, handler FrameHandler) {
	s.register(topic, handler, false)
}

// RegisterEventHandler routes every event frame to the handler, reconnecting a live stream to subscribe events
func (s *StreamClient) RegisterEventHandler(handler FrameHandler) {
	s.mu.Lock()
	fresh := s.events == nil
	s.events = handler
	conn := s.conn
	if fresh && conn != nil {
		s.resubscribe = true
	}
	s.mu.Unlock()
	if fresh && conn != nil {
		_ = conn.Close()
	}
}

// Stores a callback route, a new topic on a live stream closing it so the reconnect subscribes the topic
func (s *StreamClient) register(topic string, handler FrameHandler, inline bool) {
	s.mu.Lock()
	_, known := s.callbacks[topic]
	s.callbacks[topic] = callbackRoute{handler: handler, inline: inline}
	conn := s.conn
	if !known && conn != nil {
		s.resubscribe = true
	}
	s.mu.Unlock()
	if !known && conn != nil {
		_ = conn.Close()
	}
}

// Subscriptions the open connection request declares
func (s *StreamClient) subscriptions() []map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var subs []map[string]string
	if s.events != nil {
		subs = append(subs, map[string]string{"type": frameTypeEvent, "topic": "*"})
	}
	for topic := range s.callbacks {
		subs = append(subs, map[string]string{"type": frameTypeCallback, "topic": topic})
	}
	return subs
}

// Websocket endpoint and one use ticket from the stream gateway
func (s *StreamClient) openConnection(ctx context.Context) (string, string, error) {
	body, err := json.Marshal(map[string]any{"clientId": s.clientID, "clientSecret": s.clientSecret,
		"subscriptions": s.subscriptions(), "ua": "icebath/" + provider.Version, "localIp": s.localIP})
	if err != nil {
		return "", "", err
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, openConnectionURL, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", provider.UserAgent("icebath"))
	resp, err := s.http.Do(req)
	if err != nil {
		return "", "", &streamError{kind: "open_network", err: fmt.Errorf("open connection failed: %w", err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", &streamError{kind: "open_network", err: fmt.Errorf("open connection failed: %w", err)}
	}
	if resp.StatusCode >= 300 {
		return "", "", &streamError{kind: "open", status: resp.StatusCode,
			err: fmt.Errorf("open connection failed with HTTP %d: %s", resp.StatusCode, clip(string(raw), 300))}
	}
	var out struct {
		Endpoint string `json:"endpoint"`
		Ticket   string `json:"ticket"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Endpoint == "" || out.Ticket == "" {
		return "", "", &streamError{kind: "open_response", err: fmt.Errorf("open connection returned no endpoint and ticket: %s", clip(string(raw), 300))}
	}
	return out.Endpoint, out.Ticket, nil
}

// Opens a connection through the stream gateway and dials its websocket
func (s *StreamClient) establish(ctx context.Context) (*websocket.Conn, error) {
	endpoint, ticket, err := s.openConnection(ctx)
	if err != nil {
		return nil, err
	}
	sep := "?"
	if strings.Contains(endpoint, "?") {
		sep = "&"
	}
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, resp, err := s.dialer.DialContext(dialCtx, endpoint+sep+"ticket="+url.QueryEscape(ticket), nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return nil, &streamError{kind: "dial", err: fmt.Errorf("websocket dial failed: %w", err)}
	}
	conn.SetReadLimit(maxFrameBytes)
	return conn, nil
}

// Reads frames until the connection ends, nil for a server disconnect or a resubscribe
func (s *StreamClient) serve(ctx context.Context, conn *websocket.Conn) error {
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
	stop := make(chan struct{})
	defer func() {
		close(stop)
		s.mu.Lock()
		if s.conn == conn {
			s.conn = nil
		}
		s.mu.Unlock()
		_ = conn.Close()
	}()
	extend := func() { _ = conn.SetReadDeadline(time.Now().Add(readIdleTimeout)) }
	extend()
	conn.SetPongHandler(func(string) error {
		extend()
		return nil
	})
	conn.SetPingHandler(func(data string) error {
		extend()
		err := conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(writeTimeout))
		if err == websocket.ErrCloseSent {
			return nil
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil
		}
		return err
	})
	go s.keepalive(conn, stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if s.takeResubscribe() {
				return nil
			}
			kind := "read"
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				kind = fmt.Sprintf("closed:%d", ce.Code)
			}
			return &streamError{kind: kind, err: fmt.Errorf("stream connection lost: %w", err)}
		}
		extend()
		var frame Frame
		if err := json.Unmarshal(raw, &frame); err != nil {
			slog.Warn(fmt.Sprintf("[Dingtalk] Unreadable stream frame: %s", clip(string(raw), 200)))
			continue
		}
		if s.route(ctx, conn, &frame) {
			return nil
		}
	}
}

// Clears and reports a pending resubscribe close
func (s *StreamClient) takeResubscribe() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.resubscribe
	s.resubscribe = false
	return pending
}

// Sends websocket pings so a dead connection fails its read deadline
func (s *StreamClient) keepalive(conn *websocket.Conn, stop <-chan struct{}) {
	t := time.NewTicker(keepaliveInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout)); err != nil {
				_ = conn.Close()
				return
			}
		}
	}
}

// Dispatches one frame to its handler and acks it, true when the server asked to disconnect
func (s *StreamClient) route(ctx context.Context, conn *websocket.Conn, f *Frame) bool {
	switch f.Type {
	case frameTypeSystem:
		s.ack(conn, f, AckOK, "OK", f.Data)
		switch f.Topic() {
		case systemTopicPing:
		case systemTopicDisconnect:
			slog.Info("[Dingtalk] Stream gateway requested a reconnect")
			return true
		default:
			slog.Warn(fmt.Sprintf("[Dingtalk] Unknown system frame topic %q", f.Topic()))
		}
	case frameTypeEvent:
		s.mu.Lock()
		handler := s.events
		s.mu.Unlock()
		if handler == nil {
			s.ack(conn, f, AckNotImplemented, "not implement", eventAckData(false, "not implement"))
			return false
		}
		go func() {
			code, msg := callHandler(ctx, handler, f)
			s.ack(conn, f, code, msg, eventAckData(code == AckOK, msg))
		}()
	case frameTypeCallback:
		s.mu.Lock()
		r, ok := s.callbacks[f.Topic()]
		s.mu.Unlock()
		if !ok {
			slog.Warn(fmt.Sprintf("[Dingtalk] Callback frame for unsubscribed topic %q", f.Topic()))
			return false
		}
		run := func() {
			code, msg := callHandler(ctx, r.handler, f)
			data, _ := json.Marshal(map[string]string{"response": msg})
			s.ack(conn, f, code, msg, string(data))
		}
		if r.inline {
			run()
		} else {
			go run()
		}
	default:
		slog.Warn(fmt.Sprintf("[Dingtalk] Unknown stream frame type %q", f.Type))
	}
	return false
}

// Handler result, a panic answering as a system exception
func callHandler(ctx context.Context, handler FrameHandler, f *Frame) (code int, msg string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error(fmt.Sprintf("[Dingtalk] Stream handler for %q panicked: %v", f.Topic(), r))
			code, msg = AckSystemException, "error"
		}
	}()
	return handler(ctx, f)
}

// Event ack data telling the gateway whether to redeliver later
func eventAckData(ok bool, msg string) string {
	status := "SUCCESS"
	if !ok {
		status = "LATER"
	}
	data, _ := json.Marshal(map[string]string{"status": status, "message": msg})
	return string(data)
}

// Writes the acknowledgement of a frame, failures logged at debug since the read loop notices a dead socket
func (s *StreamClient) ack(conn *websocket.Conn, f *Frame, code int, message, data string) {
	payload := ackFrame{Code: code, Message: message, Data: data,
		Headers: map[string]string{"contentType": "application/json", "messageId": f.MessageID()}}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := conn.WriteJSON(payload); err != nil {
		slog.Debug(fmt.Sprintf("[Dingtalk] Ack of frame %s failed: %v", f.MessageID(), err))
	}
}

// Closes the live connection so its serve loop returns
func (s *StreamClient) closeConn() {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn != nil {
		s.writeMu.Lock()
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		s.writeMu.Unlock()
		_ = conn.Close()
	}
}

// LAN address the open request reports, the source address of a route to a public host
func lanIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP.String()
	}
	return ""
}

// Text cut to at most n runes for logs and error messages
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
