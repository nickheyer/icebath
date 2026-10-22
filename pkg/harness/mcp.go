package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/tool"
	"github.com/nickheyer/icebath/pkg/tool/mcp"
)

// One MCP server as configuration names it, over stdio when a command is given and streamable HTTP when a URL is
type ServerConfig struct {
	Name    string            `json:"name"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Dir     string            `json:"dir,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Seconds each handshake and listing may take
	Timeout int `json:"timeout,omitempty"`
	// Starts disabled, connected only on request
	Disabled bool `json:"disabled,omitempty"`
}

// Dialer for the transport the configuration names
func (c ServerConfig) Dialer() (mcp.Dialer, error) {
	switch {
	case c.Command != "" && c.URL != "":
		return nil, fmt.Errorf("mcp server %s: give a command or a url, not both", c.Name)
	case c.Command != "":
		var env []string
		for k, v := range c.Env {
			env = append(env, k+"="+v)
		}
		sort.Strings(env)
		return mcp.Stdio{Command: c.Command, Args: append([]string(nil), c.Args...), Env: env, Dir: c.Dir}, nil
	case c.URL != "":
		h := http.Header{}
		for k, v := range c.Headers {
			h.Set(k, v)
		}
		return mcp.HTTP{URL: c.URL, Header: h}, nil
	}
	return nil, fmt.Errorf("mcp server %s: a command or a url is required", c.Name)
}

// Connection and tool state of one managed server
type ServerStatus struct {
	Name     string
	Config   ServerConfig
	State    mcp.State
	Err      error
	Tools    []string
	Attempts int
	Info     mcp.ServerInfo
	Since    time.Time
}

// One line a server logged
type LogLine struct {
	Time   time.Time
	Server string
	Level  string
	Logger string
	Text   string
}

type managed struct {
	cfg    ServerConfig
	server *mcp.Server
	state  mcp.State
	err    error
	tools  []string
	since  time.Time
}

// Keeps MCP servers connected and their tools registered, swapping tools off the callback goroutine
type MCP struct {
	send     Sender
	registry *tool.Registry
	Options  mcp.Options
	mu       sync.Mutex
	servers  map[string]*managed
	order    []string
	Clock    func() time.Time
	wg       sync.WaitGroup
}

func NewMCP(send Sender, registry *tool.Registry) *MCP {
	return &MCP{send: send, registry: registry, servers: map[string]*managed{}, Clock: time.Now}
}

// Adds a server and connects it in the background unless it starts disabled
func (m *MCP) Add(ctx context.Context, cfg ServerConfig) error {
	if strings.TrimSpace(cfg.Name) == "" {
		return errors.New("mcp server needs a name")
	}
	if _, err := cfg.Dialer(); err != nil {
		return err
	}
	m.mu.Lock()
	if _, dup := m.servers[cfg.Name]; dup {
		m.mu.Unlock()
		return fmt.Errorf("mcp server %s already added", cfg.Name)
	}
	ms := &managed{cfg: cfg, state: mcp.StateDisconnected, since: m.Clock()}
	m.servers[cfg.Name] = ms
	m.order = append(m.order, cfg.Name)
	m.mu.Unlock()
	if cfg.Disabled {
		return nil
	}
	return m.Connect(ctx, cfg.Name)
}

// Opens the server in the background, reporting state changes as messages
func (m *MCP) Connect(ctx context.Context, name string) error {
	m.mu.Lock()
	ms, ok := m.servers[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("mcp server %s is not configured", name)
	}
	if ms.server != nil {
		m.mu.Unlock()
		return fmt.Errorf("mcp server %s is already open", name)
	}
	dialer, err := ms.cfg.Dialer()
	if err != nil {
		m.mu.Unlock()
		return err
	}
	srv := &mcp.Server{Name: name, Dialer: dialer, Options: m.Options, Timeout: time.Duration(ms.cfg.Timeout) * time.Second}
	ms.server = srv
	ms.state = mcp.StateConnecting
	ms.since = m.Clock()
	m.mu.Unlock()
	srv.OnState(func(st mcp.State, err error) {
		m.mu.Lock()
		ms.state, ms.err, ms.since = st, err, m.Clock()
		tools := append([]string(nil), ms.tools...)
		m.mu.Unlock()
		m.send(MCPMsg{Server: name, State: st, Err: err, Tools: tools})
	})
	srv.OnTools(func(tools []tool.Tool) {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.swap(name, tools)
		}()
	})
	srv.OnLog(func(level, logger string, data json.RawMessage) {
		m.logLine(name, level, logger, data)
	})
	m.send(MCPMsg{Server: name, State: mcp.StateConnecting})
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if err := srv.Open(ctx); err != nil {
			m.mu.Lock()
			ms.state, ms.err, ms.server, ms.since = mcp.StateDisconnected, err, nil, m.Clock()
			m.mu.Unlock()
			m.send(MCPMsg{Server: name, State: mcp.StateDisconnected, Err: err})
		}
	}()
	return nil
}

// Replaces the tools registered for a server with the live set
func (m *MCP) swap(name string, tools []tool.Tool) {
	m.mu.Lock()
	ms, ok := m.servers[name]
	if !ok {
		m.mu.Unlock()
		return
	}
	old := ms.tools
	m.mu.Unlock()
	for _, n := range old {
		m.registry.Remove(n)
	}
	names := make([]string, 0, len(tools))
	var failed error
	for _, t := range tools {
		if err := m.registry.Add(t); err != nil {
			failed = errors.Join(failed, err)
			continue
		}
		names = append(names, t.Definition().Name)
	}
	m.mu.Lock()
	ms.tools = names
	st, err := ms.state, ms.err
	if failed != nil {
		err = failed
		ms.err = failed
	}
	m.mu.Unlock()
	m.send(MCPMsg{Server: name, State: st, Err: err, Tools: append([]string(nil), names...)})
}

func (m *MCP) logLine(server, level, logger string, data json.RawMessage) {
	text := string(data)
	var s string
	if json.Unmarshal(data, &s) == nil {
		text = s
	}
	m.send(MCPLogMsg{Line: LogLine{Time: m.Clock(), Server: server, Level: level, Logger: logger, Text: text}})
}

// Closes the server and opens it again
func (m *MCP) Reconnect(ctx context.Context, name string) error {
	if err := m.Disconnect(name); err != nil {
		return err
	}
	return m.Connect(ctx, name)
}

// Closes the server, dropping its tools from the registry
func (m *MCP) Disconnect(name string) error {
	m.mu.Lock()
	ms, ok := m.servers[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("mcp server %s is not configured", name)
	}
	srv := ms.server
	ms.server = nil
	old := ms.tools
	ms.tools = nil
	ms.state, ms.err, ms.since = mcp.StateClosed, nil, m.Clock()
	m.mu.Unlock()
	for _, n := range old {
		m.registry.Remove(n)
	}
	if srv != nil {
		srv.Close()
	}
	m.send(MCPMsg{Server: name, State: mcp.StateClosed})
	return nil
}

// Forgets a server after closing it
func (m *MCP) Remove(name string) error {
	if err := m.Disconnect(name); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.servers, name)
	for i, n := range m.order {
		if n == name {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
	return nil
}

// State of every server in configuration order
func (m *MCP) Status() []ServerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ServerStatus, 0, len(m.order))
	for _, name := range m.order {
		ms := m.servers[name]
		st := ServerStatus{Name: name, Config: ms.cfg, State: ms.state, Err: ms.err, Tools: append([]string(nil), ms.tools...), Since: ms.since}
		if ms.server != nil {
			st.Attempts = ms.server.Attempts()
			st.Info = ms.server.Info()
		}
		out = append(out, st)
	}
	return out
}

// Closes every server and waits for background work to finish
func (m *MCP) Close() {
	m.mu.Lock()
	names := append([]string(nil), m.order...)
	m.mu.Unlock()
	for _, n := range names {
		m.Disconnect(n)
	}
	m.wg.Wait()
}
