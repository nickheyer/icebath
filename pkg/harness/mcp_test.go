package harness

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tool"
	"github.com/nickheyer/icebath/pkg/tool/mcp"
)

// In process MCP server answering the handshake and tool listing over a channel transport
type fakeServer struct {
	tools []string
	fail  bool
	mu    sync.Mutex
	dials int
	logs  chan struct{}
	conns []*fakeConn
}

type fakeConn struct {
	srv    *fakeServer
	in     chan []byte
	mu     sync.Mutex
	err    error
	closed bool
}

func (f *fakeServer) Dial(ctx context.Context) (mcp.Transport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dials++
	if f.fail {
		return nil, errors.New("refused")
	}
	c := &fakeConn{srv: f, in: make(chan []byte, 64)}
	f.conns = append(f.conns, c)
	return c, nil
}

func (c *fakeConn) Receive() <-chan []byte { return c.in }

func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.in)
	}
	return nil
}

func (c *fakeConn) deliver(v any) {
	raw, _ := json.Marshal(v)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.in <- raw
	}
}

func (c *fakeConn) Send(ctx context.Context, msg []byte) error {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(msg, &req); err != nil {
		return err
	}
	switch req.Method {
	case "initialize":
		var p struct {
			Version string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		c.deliver(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": p.Version, "capabilities": map[string]any{"tools": map[string]any{"listChanged": true}, "logging": map[string]any{}}, "serverInfo": map[string]any{"name": "fake", "version": "1"}}})
	case "tools/list":
		c.srv.mu.Lock()
		names := append([]string(nil), c.srv.tools...)
		c.srv.mu.Unlock()
		var defs []map[string]any
		for _, n := range names {
			defs = append(defs, map[string]any{"name": n, "description": "d", "inputSchema": map[string]any{"type": "object"}})
		}
		c.deliver(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": defs}})
	case "ping":
		c.deliver(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
	}
	return nil
}

// Pushes a tools changed notification and a log line through every live connection
func (f *fakeServer) change(tools ...string) {
	f.mu.Lock()
	f.tools = tools
	conns := append([]*fakeConn(nil), f.conns...)
	f.mu.Unlock()
	for _, c := range conns {
		c.deliver(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
		c.deliver(map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{"level": "info", "logger": "fake", "data": "tools changed"}})
	}
}

func TestServerConfigDialers(t *testing.T) {
	if _, err := (ServerConfig{Name: "x"}).Dialer(); err == nil {
		t.Fatal("no transport accepted")
	}
	if _, err := (ServerConfig{Name: "x", Command: "a", URL: "http://b"}).Dialer(); err == nil {
		t.Fatal("two transports accepted")
	}
	d, err := (ServerConfig{Name: "x", Command: "srv", Args: []string{"--a"}, Env: map[string]string{"B": "2", "A": "1"}, Dir: "/d"}).Dialer()
	if err != nil {
		t.Fatal(err)
	}
	st := d.(mcp.Stdio)
	if st.Command != "srv" || len(st.Args) != 1 || len(st.Env) != 2 || st.Env[0] != "A=1" || st.Dir != "/d" {
		t.Fatalf("%+v", st)
	}
	d, err = (ServerConfig{Name: "x", URL: "http://h", Headers: map[string]string{"Authorization": "Bearer t"}}).Dialer()
	if err != nil {
		t.Fatal(err)
	}
	if h := d.(mcp.HTTP); h.URL != "http://h" || h.Header.Get("Authorization") != "Bearer t" {
		t.Fatalf("%+v", h)
	}
}

func TestMCPManagerRegistersToolsOffTheCallback(t *testing.T) {
	s := newSink()
	reg := &tool.Registry{}
	j := NewJournal()
	m := NewMCP(j.Tap(s.send), reg)
	ctx := context.Background()
	if err := m.Add(ctx, ServerConfig{Name: ""}); err == nil {
		t.Fatal("unnamed server accepted")
	}
	if err := m.Add(ctx, ServerConfig{Name: "bad"}); err == nil {
		t.Fatal("server without transport accepted")
	}
	srv := &fakeServer{tools: []string{"echo"}}
	cfg := ServerConfig{Name: "fake", Command: "placeholder"}
	m.mu.Lock()
	m.servers["fake"] = &managed{cfg: cfg, state: mcp.StateDisconnected}
	m.order = append(m.order, "fake")
	m.mu.Unlock()
	if err := m.Add(ctx, cfg); err == nil {
		t.Fatal("duplicate accepted")
	}
	dialer := func(name string) error {
		m.mu.Lock()
		ms := m.servers[name]
		m.mu.Unlock()
		server := &mcp.Server{Name: name, Dialer: srv, Options: m.Options}
		m.mu.Lock()
		ms.server = server
		ms.state = mcp.StateConnecting
		m.mu.Unlock()
		server.OnState(func(st mcp.State, err error) {
			m.mu.Lock()
			ms.state, ms.err = st, err
			m.mu.Unlock()
			m.send(MCPMsg{Server: name, State: st, Err: err})
		})
		server.OnTools(func(tools []tool.Tool) { go m.swap(name, tools) })
		server.OnLog(func(level, logger string, data json.RawMessage) { m.logLine(name, level, logger, data) })
		return server.Open(ctx)
	}
	if err := dialer("fake"); err != nil {
		t.Fatal(err)
	}
	s.wait(t, func(a any) bool { mm, ok := a.(MCPMsg); return ok && len(mm.Tools) == 1 })
	if names := reg.Names(); len(names) != 1 || names[0] != "mcp__fake__echo" {
		t.Fatalf("%v", names)
	}
	srv.change("echo", "sum")
	s.wait(t, func(a any) bool { mm, ok := a.(MCPMsg); return ok && len(mm.Tools) == 2 })
	if reg.Len() != 2 {
		t.Fatalf("registry holds %d tools", reg.Len())
	}
	logged := func() bool {
		for _, line := range j.Lines() {
			if line.Source == "mcp fake" && line.Text == "tools changed" && line.Level == LevelInfo {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(5 * time.Second)
	for !logged() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !logged() {
		t.Fatalf("server log missing from the journal %+v", j.Lines())
	}
	srv.change("sum")
	s.wait(t, func(a any) bool {
		mm, ok := a.(MCPMsg)
		return ok && len(mm.Tools) == 1 && mm.Tools[0] == "mcp__fake__sum"
	})
	if names := reg.Names(); len(names) != 1 || names[0] != "mcp__fake__sum" {
		t.Fatalf("stale tools must be removed: %v", names)
	}
	status := m.Status()
	if len(status) != 1 || status[0].State != mcp.StateConnected || status[0].Info.Name != "fake" || len(status[0].Tools) != 1 {
		t.Fatalf("%+v", status)
	}
	if err := m.Disconnect("fake"); err != nil {
		t.Fatal(err)
	}
	if reg.Len() != 0 || m.Status()[0].State != mcp.StateClosed {
		t.Fatalf("disconnect must drop tools, registry %d state %s", reg.Len(), m.Status()[0].State)
	}
	if err := m.Disconnect("nope"); err == nil {
		t.Fatal("unknown server disconnected")
	}
	if err := m.Connect(ctx, "nope"); err == nil {
		t.Fatal("unknown server connected")
	}
	if err := m.Remove("fake"); err != nil {
		t.Fatal(err)
	}
	if len(m.Status()) != 0 {
		t.Fatal("removed server still listed")
	}
	if err := m.Remove("fake"); err == nil {
		t.Fatal("removing twice accepted")
	}
	m.Close()
}

func TestMCPManagerReportsFailedOpens(t *testing.T) {
	s := newSink()
	reg := &tool.Registry{}
	m := NewMCP(s.send, reg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Add(ctx, ServerConfig{Name: "dead", Command: "icebath-no-such-binary-xyz", Timeout: 1}); err != nil {
		t.Fatal(err)
	}
	s.wait(t, func(a any) bool { mm, ok := a.(MCPMsg); return ok && mm.State == mcp.StateConnecting })
	got := s.wait(t, func(a any) bool { mm, ok := a.(MCPMsg); return ok && mm.State == mcp.StateDisconnected }).(MCPMsg)
	if got.Err == nil {
		t.Fatal("failed open must carry its error")
	}
	if st := m.Status(); st[0].State != mcp.StateDisconnected || st[0].Err == nil {
		t.Fatalf("%+v", st)
	}
	if err := m.Connect(ctx, "dead"); err != nil {
		t.Fatalf("a disconnected server may be opened again: %v", err)
	}
	s.wait(t, func(a any) bool { mm, ok := a.(MCPMsg); return ok && mm.State == mcp.StateDisconnected })
	if err := m.Add(ctx, ServerConfig{Name: "later", Command: "icebath-no-such-binary-xyz", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if st := m.Status(); len(st) != 2 || st[1].State != mcp.StateDisconnected || st[1].Err != nil {
		t.Fatalf("disabled servers must wait: %+v", st)
	}
	m.Close()
}
