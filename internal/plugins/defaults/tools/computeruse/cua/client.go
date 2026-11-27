package cua

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/nickheyer/icebath/internal/mcp"
)

// The stream to the driver's MCP server is gone
var errClosed = errors.New("the cua-driver MCP stream is closed")

// Last lines a child wrote on stderr, stripped and kept twenty at most, each logged at debug after the label when it has one
type tail struct {
	label   string
	mu      sync.Mutex
	lines   []string
	partial []byte
}

func (t *tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.partial = append(t.partial, b...)
	for {
		i := strings.IndexAny(string(t.partial), "\r\n")
		if i < 0 {
			break
		}
		line := strings.TrimSpace(textOf(t.partial[:i]))
		next := i + 1
		if t.partial[i] == '\r' && next < len(t.partial) && t.partial[next] == '\n' {
			next++
		}
		t.partial = t.partial[next:]
		if line != "" {
			if t.label != "" {
				slog.Debug(t.label + line)
			}
			t.lines = append(t.lines, line)
			t.lines = t.lines[max(0, len(t.lines)-20):]
		}
	}
	return len(b), nil
}

// The last n lines
func (t *tail) last(n int) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.lines[max(0, len(t.lines)-n):])
}

// A client of the driver's MCP server on its stdio: requests matched to answers by id, pings answered, other requests of the server refused
type client struct {
	t       *mcp.StdioTransport
	stderr  *tail
	mu      sync.Mutex
	pending map[string]chan *mcp.Message
	closed  chan struct{}
	once    sync.Once
}

// Starts the server, its process group owned so it dies with icebath
func open(command string, args []string, env map[string]string, owner mcp.Owner) (*client, error) {
	stderr := &tail{}
	t, err := mcp.StartStdio(mcp.StdioParams{Command: command, Args: args, Env: env, Stderr: stderr, Owner: owner})
	if err != nil {
		return nil, err
	}
	c := &client{t: t, stderr: stderr, pending: map[string]chan *mcp.Message{}, closed: make(chan struct{})}
	go c.pump()
	return c, nil
}

func (c *client) pump() {
	for msg := range c.t.Receive() {
		switch {
		case msg.IsResponse():
			c.mu.Lock()
			ch := c.pending[mcp.IDKey(msg.ID)]
			delete(c.pending, mcp.IDKey(msg.ID))
			c.mu.Unlock()
			if ch != nil {
				ch <- msg
			}
		case msg.IsRequest():
			reply := mcp.NewErrorResponse(msg.ID, -32601, "Method not found")
			if msg.Method == "ping" {
				reply, _ = mcp.NewResponse(msg.ID, map[string]any{})
			}
			c.t.Send(context.Background(), reply)
		}
	}
	c.once.Do(func() { close(c.closed) })
}

// Result of one request, errClosed when the stream is gone
func (c *client) request(ctx context.Context, method string, params any, out any) error {
	req, err := mcp.NewRequest(method, params)
	if err != nil {
		return err
	}
	ch := make(chan *mcp.Message, 1)
	id := mcp.IDKey(req.ID)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	select {
	case <-c.closed:
		return errClosed
	default:
	}
	if err := c.t.Send(ctx, req); err != nil {
		if errors.Is(err, syscall.EPIPE) || errors.Is(err, io.ErrClosedPipe) {
			return errClosed
		}
		return err
	}
	select {
	case msg := <-ch:
		if msg.Error != nil {
			return errors.New(msg.Error.Message)
		}
		if out == nil || len(msg.Result) == 0 {
			return nil
		}
		return json.Unmarshal(msg.Result, out)
	case <-c.closed:
		return errClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Handshake at the newest revision, refusing a server that answers with one icebath does not speak
func (c *client) initialize(ctx context.Context) (map[string]any, error) {
	params := map[string]any{"protocolVersion": mcp.LatestVersion, "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "mcp", "version": "0.1.0"}}
	var result map[string]any
	if err := c.request(ctx, "initialize", params, &result); err != nil {
		return nil, err
	}
	version, _ := result["protocolVersion"].(string)
	if version != mcp.LatestVersion && !slices.Contains(mcp.HandshakeVersions, version) {
		if raw, ok := result["protocolVersion"]; ok && raw != nil {
			return nil, fmt.Errorf("Unsupported protocol version from the server: %v", raw)
		}
		return nil, errors.New("The server reported no protocol version")
	}
	note, err := mcp.NewNotification("notifications/initialized", nil)
	if err != nil {
		return nil, err
	}
	return result, c.t.Send(ctx, note)
}

// The tools/list result as the server sent it
func (c *client) tools(ctx context.Context) (map[string]any, error) {
	var result map[string]any
	return result, c.request(ctx, "tools/list", nil, &result)
}

// The tools/call result as the server sent it
func (c *client) call(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	if args == nil {
		args = map[string]any{}
	}
	var result map[string]any
	return result, c.request(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &result)
}

func (c *client) close() error {
	err := c.t.Close()
	c.once.Do(func() { close(c.closed) })
	return err
}
