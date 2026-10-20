package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/tool"
)

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
	// First wait before a reconnect attempt, one second when zero
	Backoff time.Duration
	// Longest wait between reconnect attempts, thirty seconds when zero
	MaxBackoff time.Duration
	// Budget for each handshake and tool listing, thirty seconds when zero
	Timeout time.Duration

	mu       sync.Mutex
	client   *Client
	state    State
	err      error
	tools    []tool.Tool
	info     ServerInfo
	onState  []func(State, error)
	onTools  []func([]tool.Tool)
	onLog    func(level, logger string, data json.RawMessage)
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	attempts int
}

func (s *Server) backoff() time.Duration {
	if s.Backoff > 0 {
		return s.Backoff
	}
	return time.Second
}

func (s *Server) maxBackoff() time.Duration {
	if s.MaxBackoff > 0 {
		return s.MaxBackoff
	}
	return 30 * time.Second
}

func (s *Server) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return 30 * time.Second
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
	cctx, cancel := context.WithTimeout(ctx, s.timeout())
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
	onLog := s.onLog
	s.mu.Unlock()
	if onLog != nil {
		c.OnLog(onLog)
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout())
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
	delay := s.backoff()
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
		delay = min(delay*2, s.maxBackoff())
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

// Routes server log notifications to fn across reconnects
func (s *Server) OnLog(fn func(level, logger string, data json.RawMessage)) {
	s.mu.Lock()
	s.onLog = fn
	c := s.client
	s.mu.Unlock()
	if c != nil {
		c.OnLog(fn)
	}
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
