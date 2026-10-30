// Model Context Protocol client that exposes the tools of a server as local tools
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/tool"
)

// Protocol revision this client speaks
const Version = "2025-06-18"

// Protocol revisions the client accepts from a server
var SupportedVersions = []string{"2024-11-05", "2025-03-26", Version}

// Error a transport reports when the server forgot the session and a new handshake is needed
var ErrSessionExpired = errors.New("mcp: server session expired")

// Time progress for a finished request keeps reaching its reporter
const progressGrace = 2 * time.Second

// Carries JSON-RPC messages between the client and one server
type Transport interface {
	Send(ctx context.Context, msg []byte) error
	Receive() <-chan []byte
	Close() error
}

// Opens a transport to a server
type Dialer interface {
	Dial(ctx context.Context) (Transport, error)
}

// Identity sent to the server during initialization
type Info struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Directory the client offers a server as a root
type Root struct {
	URI  string `json:"uri"`
	Name string `json:"name,omitempty"`
}

// What the client declares to a server
type Options struct {
	// Roots served to the server, declaring the roots capability when non empty
	Roots []Root
}

// What the server said about itself during initialization
type ServerInfo struct {
	Name            string
	Version         string
	ProtocolVersion string
	Instructions    string
	// Offers tools
	Tools bool
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Failure reported by the server for one request
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("mcp: %s (code %d)", e.Message, e.Code) }

type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      Info           `json:"clientInfo"`
}

type initializeResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	Capabilities    struct {
		Tools *struct{} `json:"tools"`
	} `json:"capabilities"`
	ServerInfo   Info   `json:"serverInfo"`
	Instructions string `json:"instructions"`
}

// Connected server with its request multiplexer
type Client struct {
	name      string
	t         Transport
	opts      Options
	mu        sync.Mutex
	next      int64
	pending   map[string]chan rpcMessage
	progress  map[string]context.Context
	closed    chan struct{}
	closeOnce sync.Once
	err       error
	server    ServerInfo
	notes     chan rpcMessage
	notesDone chan struct{}
	onTools   func()
}

// Dials the server, performs the initialize handshake and starts reading
func Connect(ctx context.Context, name string, d Dialer, o Options) (*Client, error) {
	t, err := d.Dial(ctx)
	if err != nil {
		return nil, err
	}
	c, err := Attach(ctx, name, t, o)
	if err != nil {
		t.Close()
		return nil, err
	}
	return c, nil
}

// Performs the initialize handshake over an open transport and starts reading
func Attach(ctx context.Context, name string, t Transport, o Options) (*Client, error) {
	c := &Client{name: name, t: t, opts: o, pending: map[string]chan rpcMessage{}, progress: map[string]context.Context{}, closed: make(chan struct{}), notes: make(chan rpcMessage, 64), notesDone: make(chan struct{})}
	go c.read()
	go c.notify()
	raw, err := c.Call(ctx, "initialize", initializeParams{ProtocolVersion: Version, Capabilities: c.capabilities(), ClientInfo: Info{Name: "icebath"}})
	if err != nil {
		c.Close()
		return nil, err
	}
	var res initializeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp: initialize result: %w", err)
	}
	if !supported(res.ProtocolVersion) {
		c.Close()
		return nil, fmt.Errorf("mcp: server speaks protocol %q, this client accepts %v", res.ProtocolVersion, SupportedVersions)
	}
	c.server = ServerInfo{Name: res.ServerInfo.Name, Version: res.ServerInfo.Version, ProtocolVersion: res.ProtocolVersion, Instructions: res.Instructions, Tools: res.Capabilities.Tools != nil}
	if err := c.Notify(ctx, "notifications/initialized", nil); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func supported(version string) bool {
	for _, v := range SupportedVersions {
		if v == version {
			return true
		}
	}
	return false
}

// Capabilities declared to the server, the roots when any are offered
func (c *Client) capabilities() map[string]any {
	caps := map[string]any{}
	if len(c.opts.Roots) > 0 {
		caps["roots"] = map[string]any{"listChanged": false}
	}
	return caps
}

// Label used in tool names, shared with managed servers
func (c *Client) Label() string { return c.name }

func (c *Client) Server() ServerInfo { return c.server }

// Channel closed once the client can no longer talk to the server
func (c *Client) Done() <-chan struct{} { return c.closed }

// Why the client stopped, nil while it is alive
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Registers the callback run when the server announces a changed tool list
func (c *Client) OnToolsChanged(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onTools = fn
}

// Sends a request and waits for its result, failing on an error response or a closed transport
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return c.call(ctx, method, params)
}

// Sends a request carrying a progress token so server progress reaches the context reporter
func (c *Client) CallWithProgress(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	c.mu.Lock()
	c.next++
	token := "p" + strconv.FormatInt(c.next, 10)
	c.progress[token] = ctx
	c.mu.Unlock()
	defer time.AfterFunc(progressGrace, func() {
		c.mu.Lock()
		delete(c.progress, token)
		c.mu.Unlock()
	})
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{"progressToken": token}
	return c.call(ctx, method, params)
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.next++
	id := strconv.FormatInt(c.next, 10)
	ch := make(chan rpcMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	msg, err := encode(rpcMessage{ID: json.RawMessage(id), Method: method}, params)
	if err == nil {
		err = c.t.Send(ctx, msg)
	}
	if err != nil {
		c.drop(id)
		if ctx.Err() != nil && c.Err() == nil {
			c.cancelled(id, ctx.Err())
		}
		return nil, err
	}
	select {
	case res := <-ch:
		if res.Error != nil {
			return nil, res.Error
		}
		return res.Result, nil
	case <-ctx.Done():
		c.drop(id)
		c.cancelled(id, ctx.Err())
		return nil, ctx.Err()
	case <-c.closed:
		c.drop(id)
		return nil, c.closedErr()
	}
}

// Tells the server a request was abandoned so it can stop working on it
func (c *Client) cancelled(id string, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reason := ""
	if cause != nil {
		reason = cause.Error()
	}
	c.Notify(ctx, "notifications/cancelled", map[string]any{"requestId": json.RawMessage(id), "reason": reason})
}

// Sends a notification, which has no response
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	msg, err := encode(rpcMessage{Method: method}, params)
	if err != nil {
		return err
	}
	return c.t.Send(ctx, msg)
}

// Stops reading and closes the transport
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.t.Close()
		c.fail(errors.New("mcp: client closed"))
	})
	return err
}

func (c *Client) drop(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Client) closedErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Records the failure once and wakes every waiting call
func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
		close(c.closed)
	}
	c.mu.Unlock()
}

// Dispatches server messages to waiting calls, request handlers and the notification queue
func (c *Client) read() {
	for raw := range c.t.Receive() {
		for _, msg := range split(raw) {
			c.dispatch(msg)
		}
	}
	if e, ok := c.t.(interface{ Err() error }); ok && e.Err() != nil {
		if errors.Is(e.Err(), ErrSessionExpired) {
			c.fail(ErrSessionExpired)
			return
		}
		c.fail(fmt.Errorf("mcp: transport closed: %w", e.Err()))
		return
	}
	c.fail(errors.New("mcp: transport closed"))
}

// Expands a JSON-RPC batch into its messages, passing single messages through
func split(raw []byte) []rpcMessage {
	var batch []rpcMessage
	if len(raw) > 0 && raw[0] == '[' && json.Unmarshal(raw, &batch) == nil {
		return batch
	}
	var one rpcMessage
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil
	}
	return []rpcMessage{one}
}

func (c *Client) dispatch(msg rpcMessage) {
	switch {
	case msg.Method == "" && len(msg.ID) > 0:
		id := string(msg.ID)
		c.mu.Lock()
		ch, ok := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if ok {
			ch <- msg
		}
	case msg.Method != "" && len(msg.ID) > 0:
		go c.answer(msg)
	case msg.Method != "":
		select {
		case c.notes <- msg:
		case <-c.closed:
		}
	}
}

// Answers the requests a server may send, pings and the roots listing
func (c *Client) answer(req rpcMessage) {
	reply := rpcMessage{ID: req.ID}
	switch req.Method {
	case "ping":
		reply.Result = json.RawMessage(`{}`)
	case "roots/list":
		if len(c.opts.Roots) == 0 {
			reply.Error = &Error{Code: -32601, Message: "method not found: roots/list"}
			break
		}
		raw, _ := json.Marshal(map[string]any{"roots": c.opts.Roots})
		reply.Result = raw
	default:
		reply.Error = &Error{Code: -32601, Message: "method not found: " + req.Method}
	}
	msg, err := encode(reply, nil)
	if err != nil {
		return
	}
	c.t.Send(context.Background(), msg)
}

// Delivers queued notifications on their own goroutine so callbacks may call back into the client
func (c *Client) notify() {
	defer close(c.notesDone)
	for {
		select {
		case msg := <-c.notes:
			c.notified(msg)
		case <-c.closed:
			return
		}
	}
}

func (c *Client) notified(msg rpcMessage) {
	c.mu.Lock()
	onTools := c.onTools
	c.mu.Unlock()
	switch msg.Method {
	case "notifications/tools/list_changed":
		if onTools != nil {
			onTools()
		}
	case "notifications/progress":
		var p struct {
			Token    json.RawMessage `json:"progressToken"`
			Progress float64         `json:"progress"`
			Total    float64         `json:"total"`
			Message  string          `json:"message"`
		}
		if json.Unmarshal(msg.Params, &p) != nil {
			return
		}
		var token string
		if json.Unmarshal(p.Token, &token) != nil {
			token = string(p.Token)
		}
		c.mu.Lock()
		ctx := c.progress[token]
		c.mu.Unlock()
		if ctx != nil {
			tool.Report(ctx, tool.Progress{Text: p.Message, Done: int(p.Progress), Total: int(p.Total)})
		}
	}
}

// Serializes a message with its params, leaving params out when nil
func encode(msg rpcMessage, params any) ([]byte, error) {
	msg.JSONRPC = "2.0"
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		msg.Params = raw
	}
	return json.Marshal(msg)
}

// Connection state of a managed server
type State string

const (
	StateConnecting   State = "connecting"
	StateConnected    State = "connected"
	StateDisconnected State = "disconnected"
	StateClosed       State = "closed"
)

// Server kept connected across failures, with tools that follow the live client
type Server struct {
	Name    string
	Dialer  Dialer
	Options Options
	// Budget for each handshake and tool listing, unlimited when zero
	Timeout time.Duration

	mu       sync.Mutex
	client   *Client
	state    State
	err      error
	tools    []tool.Tool
	info     ServerInfo
	onState  []func(State, error)
	onTools  []func([]tool.Tool)
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	attempts int
}

// Context for one handshake or listing, ending after the server timeout when one is set
func (s *Server) within(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.Timeout > 0 {
		return context.WithTimeout(ctx, s.Timeout)
	}
	return context.WithCancel(ctx)
}

// Connects, lists the tools and starts watching the connection, failing when the first connect fails
func (s *Server) Open(ctx context.Context) error {
	if s.Dialer == nil {
		return errors.New("mcp: server dialer is required")
	}
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		return errors.New("mcp: server already open")
	}
	sctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.state = StateConnecting
	s.mu.Unlock()
	c, err := s.connect(ctx)
	if err != nil {
		s.mu.Lock()
		s.cancel = nil
		s.state = StateDisconnected
		s.err = err
		s.mu.Unlock()
		cancel()
		return err
	}
	s.adopt(c)
	s.wg.Add(1)
	go s.supervise(sctx)
	return nil
}

// Dials, handshakes and lists tools under the server timeout
func (s *Server) connect(ctx context.Context) (*Client, error) {
	cctx, cancel := s.within(ctx)
	defer cancel()
	c, err := Connect(cctx, s.Name, s.Dialer, s.Options)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Takes a fresh client into service, wiring callbacks and publishing its tools
func (s *Server) adopt(c *Client) {
	c.OnToolsChanged(func() {
		go s.refresh(c)
	})
	s.mu.Lock()
	s.client = c
	s.info = c.Server()
	s.state = StateConnected
	s.err = nil
	s.attempts = 0
	s.mu.Unlock()
	s.refresh(c)
	s.announce(StateConnected, nil)
}

// Lists the tools of a client and publishes them when the set changed
func (s *Server) refresh(c *Client) {
	ctx, cancel := s.within(context.Background())
	defer cancel()
	var tools []tool.Tool
	if c.Server().Tools {
		defs, err := c.listTools(ctx)
		if err != nil {
			return
		}
		tools = make([]tool.Tool, len(defs))
		for i, d := range defs {
			tools[i] = wrap(s, d)
		}
	}
	s.mu.Lock()
	if s.client != c {
		s.mu.Unlock()
		return
	}
	changed := !sameTools(s.tools, tools)
	s.tools = tools
	fns := append([]func([]tool.Tool){}, s.onTools...)
	s.mu.Unlock()
	if !changed {
		return
	}
	for _, fn := range fns {
		fn(tools)
	}
}

func sameTools(a, b []tool.Tool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		da, db := a[i].Definition(), b[i].Definition()
		if da.Name != db.Name || da.Description != db.Description || string(da.Parameters) != string(db.Parameters) || tool.Describe(a[i]) != tool.Describe(b[i]) {
			return false
		}
	}
	return true
}

// Watches the live client and reconnects with backoff whenever it fails
func (s *Server) supervise(ctx context.Context) {
	defer s.wg.Done()
	for {
		s.mu.Lock()
		c := s.client
		s.mu.Unlock()
		if c == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-c.Done():
		}
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		s.state = StateDisconnected
		s.err = c.Err()
		s.mu.Unlock()
		s.announce(StateDisconnected, c.Err())
		if !s.reconnect(ctx) {
			return
		}
	}
}

// Tries to connect again until it works or the server is closed
func (s *Server) reconnect(ctx context.Context) bool {
	delay := time.Second
	for {
		s.mu.Lock()
		s.state = StateConnecting
		s.attempts++
		s.mu.Unlock()
		s.announce(StateConnecting, nil)
		c, err := s.connect(ctx)
		if err == nil {
			s.adopt(c)
			return true
		}
		s.mu.Lock()
		s.state = StateDisconnected
		s.err = err
		s.mu.Unlock()
		s.announce(StateDisconnected, err)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return false
		}
		delay = min(delay*2, 30*time.Second)
	}
}

func (s *Server) announce(state State, err error) {
	s.mu.Lock()
	fns := append([]func(State, error){}, s.onState...)
	s.mu.Unlock()
	for _, fn := range fns {
		fn(state, err)
	}
}

// Registers a callback run on every connection state change
func (s *Server) OnState(fn func(State, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onState = append(s.onState, fn)
}

// Registers a callback run with the new tool set whenever it changes
func (s *Server) OnTools(fn func([]tool.Tool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onTools = append(s.onTools, fn)
}

// Current state with the error behind a disconnect
func (s *Server) State() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.err
}

// Reconnect attempts made since the last successful connection
func (s *Server) Attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

// Live client, nil while disconnected
func (s *Server) Client() *Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateConnected {
		return nil
	}
	return s.client
}

// What the server said about itself at the last handshake
func (s *Server) Info() ServerInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

// Tools as last listed, each routed through whichever client is live when it runs
func (s *Server) Tools() []tool.Tool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]tool.Tool(nil), s.tools...)
}

// Label used in tool names
func (s *Server) Label() string { return s.Name }

// Sends a request through the live client, failing while disconnected
func (s *Server) CallWithProgress(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	c := s.Client()
	if c == nil {
		_, err := s.State()
		if err == nil {
			err = errors.New("mcp: server " + s.Name + " is not connected")
		}
		return nil, err
	}
	return c.CallWithProgress(ctx, method, params)
}

// Sends a plain request through the live client
func (s *Server) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c := s.Client()
	if c == nil {
		_, err := s.State()
		if err == nil {
			err = errors.New("mcp: server " + s.Name + " is not connected")
		}
		return nil, err
	}
	return c.Call(ctx, method, params)
}

// Stops supervising and closes the live client
func (s *Server) Close() error {
	s.mu.Lock()
	cancel := s.cancel
	c := s.client
	s.cancel = nil
	s.client = nil
	s.state = StateClosed
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	var err error
	if c != nil {
		err = c.Close()
	}
	s.wg.Wait()
	s.announce(StateClosed, nil)
	return err
}
