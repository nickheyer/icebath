package slacktest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

// One Web API request the fake answered
type Call struct {
	Method string
	Args   map[string]any
	Header http.Header
}

// Loopback Slack with the Web API every slack.com host dials and a Socket Mode websocket endpoint
type Server struct {
	api       *httptest.Server
	socket    *httptest.Server
	mu        sync.Mutex
	calls     []Call
	responses map[string][]reply
	uploads   int
	tickets   int
	conns     []*Conn
	arrivals  chan *Conn
	callSeen  chan struct{}
	apiConns  map[net.Conn]http.ConnState
}

// Starts the fake and routes every slack.com dial of the default transport to it until the test ends
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{responses: map[string][]reply{}, arrivals: make(chan *Conn, 64), callSeen: make(chan struct{}, 1),
		apiConns: map[net.Conn]http.ConnState{}}
	s.api = httptest.NewUnstartedServer(http.HandlerFunc(s.serveAPI))
	s.api.Config.ConnState = func(c net.Conn, state http.ConnState) {
		s.mu.Lock()
		if state == http.StateClosed || state == http.StateHijacked {
			delete(s.apiConns, c)
		} else {
			s.apiConns[c] = state
		}
		s.mu.Unlock()
	}
	s.api.StartTLS()
	s.socket = httptest.NewServer(http.HandlerFunc(s.serveSocket))
	dt := http.DefaultTransport.(*http.Transport)
	prevDial, prevTLS := dt.DialContext, dt.TLSClientConfig
	tlsConf := s.api.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsConf.ServerName = "example.com"
	routeSlackDials(dt, s.api.Listener.Addr().String(), tlsConf)
	t.Cleanup(func() {
		dt.DialContext, dt.TLSClientConfig = prevDial, prevTLS
		s.Close()
	})
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(k, "")
	}
	return s
}

// Names the loopback Web API address a child process routes slack.com to
const childAddrEnv = "ICEBATH_TEST_SLACK_API_ADDR"

// Names the PEM file of the certificate the loopback Web API presents to a child process
const childCAEnv = "ICEBATH_TEST_SLACK_API_CA"

// Environment entries that let a child process reach this fake through RouteFromEnv, the certificate written under dir
func (s *Server) ChildEnv(dir string) ([]string, error) {
	path := filepath.Join(dir, "slack-fake-ca.pem")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.api.Certificate().Raw})
	if err := os.WriteFile(path, block, 0o600); err != nil {
		return nil, err
	}
	return []string{childAddrEnv + "=" + s.api.Listener.Addr().String(), childCAEnv + "=" + path}, nil
}

// Blocks external network in a child process and routes its slack.com dials to the parent's fake
func RouteFromEnv() {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	addr, caPath := os.Getenv(childAddrEnv), os.Getenv(childCAEnv)
	raw, err := os.ReadFile(caPath)
	if addr == "" || err != nil {
		panic(fmt.Sprintf("slacktest: %s and %s must name the parent's fake: %v", childAddrEnv, childCAEnv, err))
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		panic("slacktest: no certificate in " + caPath)
	}
	dt := http.DefaultTransport.(*http.Transport)
	routeSlackDials(dt, addr, &tls.Config{RootCAs: pool, ServerName: "example.com"})
}

// Sends slack.com dials of the transport to addr under tlsConf and every other dial to the previous dialer
func routeSlackDials(dt *http.Transport, addr string, tlsConf *tls.Config) {
	prevDial := dt.DialContext
	dt.DialContext = func(ctx context.Context, network, target string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(target)
		if host == "slack.com" || strings.HasSuffix(host, ".slack.com") {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
		if prevDial != nil {
			return prevDial(ctx, network, target)
		}
		return (&net.Dialer{}).DialContext(ctx, network, target)
	}
	dt.TLSClientConfig = tlsConf
}

// Stops both endpoints and drops every open socket
func (s *Server) Close() {
	s.mu.Lock()
	conns := append([]*Conn(nil), s.conns...)
	s.mu.Unlock()
	for _, c := range conns {
		c.Drop()
	}
	s.api.Close()
	s.socket.CloseClientConnections()
	s.socket.Close()
}

// One queued answer with its HTTP status, headers and JSON body
type reply struct {
	status int
	header map[string]string
	body   map[string]any
	delay  time.Duration
}

// Queues a payload for the method, the last queued payload repeating
func (s *Server) Respond(method string, payload map[string]any) {
	s.RespondStatus(method, http.StatusOK, nil, payload)
}

// Queues a payload the method answers only after the delay
func (s *Server) RespondAfter(method string, delay time.Duration, payload map[string]any) {
	s.mu.Lock()
	s.responses[method] = append(s.responses[method], reply{status: http.StatusOK, body: payload, delay: delay})
	s.mu.Unlock()
}

// Queues an answer with an HTTP status and headers for the method, the last queued answer repeating
func (s *Server) RespondStatus(method string, status int, header map[string]string, payload map[string]any) {
	s.mu.Lock()
	s.responses[method] = append(s.responses[method], reply{status: status, header: header, body: payload})
	s.mu.Unlock()
}

// Calls recorded for the method in order
func (s *Server) CallsTo(method string) []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Call
	for _, c := range s.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// Every recorded call in order
func (s *Server) AllCalls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Waits until the method has been called n times and returns those calls
func (s *Server) WaitCalls(method string, n int, timeout time.Duration) ([]Call, error) {
	deadline := time.After(timeout)
	for {
		if calls := s.CallsTo(method); len(calls) >= n {
			return calls, nil
		}
		select {
		case <-s.callSeen:
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			return s.CallsTo(method), fmt.Errorf("%s called %d times, want %d", method, len(s.CallsTo(method)), n)
		}
	}
}

// Web API connections clients still hold open, idle keep-alives included
func (s *Server) OpenAPIConns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.apiConns)
}

// Fresh Socket Mode websocket URL carrying its own ticket, the answer apps.connections.open gives
func (s *Server) SocketURL() string {
	s.mu.Lock()
	s.tickets++
	n := s.tickets
	s.mu.Unlock()
	return fmt.Sprintf("ws://%s/link/?ticket=t%d&app_id=A_FAKE", s.socket.Listener.Addr().String(), n)
}

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/upload/") {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
		return
	}
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	args := map[string]any{}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &args)
	} else {
		_ = r.ParseForm()
		for k, v := range r.Form {
			args[k] = v[0]
		}
	}
	s.mu.Lock()
	s.calls = append(s.calls, Call{Method: method, Args: args, Header: r.Header.Clone()})
	var payload map[string]any
	answer := reply{status: http.StatusOK}
	if queue := s.responses[method]; len(queue) > 0 {
		answer = queue[0]
		payload = answer.body
		if len(queue) > 1 {
			s.responses[method] = queue[1:]
		}
	}
	if method == "files.getUploadURLExternal" && payload == nil {
		s.uploads++
		id := fmt.Sprintf("F%d", s.uploads)
		payload = map[string]any{"ok": true, "upload_url": "https://files.slack.com/upload/" + id, "file_id": id}
	}
	s.mu.Unlock()
	if method == "apps.connections.open" && payload == nil {
		payload = map[string]any{"ok": true, "url": s.SocketURL()}
	}
	if payload == nil {
		payload = map[string]any{"ok": true}
	}
	select {
	case s.callSeen <- struct{}{}:
	default:
	}
	if answer.delay > 0 {
		select {
		case <-time.After(answer.delay):
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	for k, v := range answer.header {
		w.Header().Set(k, v)
	}
	w.WriteHeader(answer.status)
	_ = json.NewEncoder(w).Encode(payload)
}

// One accepted Socket Mode connection with the acknowledgements the client wrote on it
type Conn struct {
	Ticket string
	ws     *websocket.Conn
	writeM sync.Mutex
	mu     sync.Mutex
	acks   map[string]map[string]any
	ackSig chan struct{}
	closed chan struct{}
	nextID int
	once   sync.Once
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func (s *Server) serveSocket(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &Conn{Ticket: r.URL.Query().Get("ticket"), ws: ws, acks: map[string]map[string]any{},
		ackSig: make(chan struct{}, 1), closed: make(chan struct{})}
	s.mu.Lock()
	s.conns = append(s.conns, c)
	s.mu.Unlock()
	if err := c.write(map[string]any{"type": "hello", "num_connections": 1,
		"debug_info":      map[string]any{"host": "applink-fake", "approximate_connection_time": 18060},
		"connection_info": map[string]any{"app_id": "A_FAKE"}}); err != nil {
		c.Drop()
		return
	}
	s.arrivals <- c
	go c.read()
}

// Every connection accepted so far in order
func (s *Server) Conns() []*Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Conn(nil), s.conns...)
}

// Waits for the next client to connect and receive its hello
func (s *Server) NextConn(timeout time.Duration) (*Conn, error) {
	select {
	case c := <-s.arrivals:
		return c, nil
	case <-time.After(timeout):
		return nil, errors.New("no Socket Mode connection arrived")
	}
}

func (c *Conn) read() {
	defer c.Drop()
	for {
		_, raw, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		var ack struct {
			EnvelopeID string         `json:"envelope_id"`
			Payload    map[string]any `json:"payload"`
		}
		if json.Unmarshal(raw, &ack) != nil || ack.EnvelopeID == "" {
			continue
		}
		c.mu.Lock()
		if ack.Payload == nil {
			ack.Payload = map[string]any{}
		}
		c.acks[ack.EnvelopeID] = ack.Payload
		c.mu.Unlock()
		select {
		case c.ackSig <- struct{}{}:
		default:
		}
	}
}

func (c *Conn) write(frame map[string]any) error {
	c.writeM.Lock()
	defer c.writeM.Unlock()
	return c.ws.WriteJSON(frame)
}

// Sends an envelope of the kind with its payload and returns the envelope id
func (c *Conn) Send(kind string, payload map[string]any) (string, error) {
	c.mu.Lock()
	c.nextID++
	id := fmt.Sprintf("%s-env-%d", c.Ticket, c.nextID)
	c.mu.Unlock()
	return id, c.write(map[string]any{"envelope_id": id, "type": kind, "payload": payload,
		"accepts_response_payload": kind != "events_api", "retry_attempt": 0, "retry_reason": ""})
}

// Sends an Events API envelope wrapping the event in an event_callback of the team
func (c *Conn) SendEvent(teamID string, event map[string]any) (string, error) {
	c.mu.Lock()
	n := c.nextID + 1
	c.mu.Unlock()
	return c.Send("events_api", map[string]any{"token": "verification", "team_id": teamID, "api_app_id": "A_FAKE",
		"event": event, "type": "event_callback", "event_id": fmt.Sprintf("Ev%s%d", c.Ticket, n),
		"event_time": time.Now().Unix(), "authorizations": []any{map[string]any{"team_id": teamID, "is_bot": true}}})
}

// Waits for the client to acknowledge the envelope and returns the acknowledgement payload
func (c *Conn) Ack(envelopeID string, timeout time.Duration) (map[string]any, error) {
	deadline := time.After(timeout)
	for {
		c.mu.Lock()
		payload, ok := c.acks[envelopeID]
		c.mu.Unlock()
		if ok {
			return payload, nil
		}
		select {
		case <-c.ackSig:
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			return nil, fmt.Errorf("envelope %s was not acknowledged", envelopeID)
		}
	}
}

// Asks the client to reconnect the way Slack rotates a socket
func (c *Conn) Disconnect(reason string) error {
	return c.write(map[string]any{"type": "disconnect", "reason": reason, "debug_info": map[string]any{"host": "applink-fake"}})
}

// Sends a websocket ping the client answers with a pong
func (c *Conn) Ping(data string) error {
	c.writeM.Lock()
	defer c.writeM.Unlock()
	return c.ws.WriteControl(websocket.PingMessage, []byte(data), time.Now().Add(time.Second))
}

// Cuts the connection without a close frame
func (c *Conn) Drop() {
	c.once.Do(func() {
		_ = c.ws.UnderlyingConn().Close()
		close(c.closed)
	})
}

// Closed once the connection is gone from either side
func (c *Conn) Done() <-chan struct{} { return c.closed }
