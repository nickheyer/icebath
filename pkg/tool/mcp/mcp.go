// Model Context Protocol client that exposes the tools of a server as local tools
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
)

// Protocol revision this client speaks
const Version = "2025-06-18"

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

// What the server said about itself during initialization
type ServerInfo struct {
	Name            string
	Version         string
	ProtocolVersion string
	Instructions    string
	Tools           bool
	ToolsChange     bool
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
		Tools *struct {
			ListChanged bool `json:"listChanged"`
		} `json:"tools"`
	} `json:"capabilities"`
	ServerInfo   Info   `json:"serverInfo"`
	Instructions string `json:"instructions"`
}

// Connected server with its request multiplexer
type Client struct {
	name      string
	t         Transport
	mu        sync.Mutex
	next      int64
	pending   map[string]chan rpcMessage
	closed    chan struct{}
	closeOnce sync.Once
	err       error
	server    ServerInfo
	onTools   func()
	onLog     func(level, logger string, data json.RawMessage)
}

// Dials the server, performs the initialize handshake and starts reading
func Connect(ctx context.Context, name string, d Dialer, info Info) (*Client, error) {
	t, err := d.Dial(ctx)
	if err != nil {
		return nil, err
	}
	c, err := Attach(ctx, name, t, info)
	if err != nil {
		t.Close()
		return nil, err
	}
	return c, nil
}

// Performs the initialize handshake over an open transport and starts reading
func Attach(ctx context.Context, name string, t Transport, info Info) (*Client, error) {
	c := &Client{name: name, t: t, pending: map[string]chan rpcMessage{}, closed: make(chan struct{})}
	go c.read()
	raw, err := c.Call(ctx, "initialize", initializeParams{ProtocolVersion: Version, Capabilities: map[string]any{}, ClientInfo: info})
	if err != nil {
		c.Close()
		return nil, err
	}
	var res initializeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp: initialize result: %w", err)
	}
	c.server = ServerInfo{Name: res.ServerInfo.Name, Version: res.ServerInfo.Version, ProtocolVersion: res.ProtocolVersion, Instructions: res.Instructions, Tools: res.Capabilities.Tools != nil}
	if res.Capabilities.Tools != nil {
		c.server.ToolsChange = res.Capabilities.Tools.ListChanged
	}
	if err := c.Notify(ctx, "notifications/initialized", nil); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Label used in tool names
func (c *Client) Name() string { return c.name }

func (c *Client) Server() ServerInfo { return c.server }

// Registers the callback run when the server announces a changed tool list
func (c *Client) OnToolsChanged(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onTools = fn
}

// Registers the callback run for every log notification from the server
func (c *Client) OnLog(fn func(level, logger string, data json.RawMessage)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onLog = fn
}

// Sends a request and waits for its result, failing on an error response or a closed transport
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
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
		return nil, ctx.Err()
	case <-c.closed:
		c.drop(id)
		return nil, c.closedErr()
	}
}

// Sends a notification, which has no response
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	msg, err := encode(rpcMessage{Method: method}, params)
	if err != nil {
		return err
	}
	return c.t.Send(ctx, msg)
}

// Checks the server is alive
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.Call(ctx, "ping", nil)
	return err
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

// Dispatches server messages to waiting calls, request handlers and notification callbacks
func (c *Client) read() {
	for raw := range c.t.Receive() {
		for _, msg := range split(raw) {
			c.dispatch(msg)
		}
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
		c.notified(msg)
	}
}

// Answers the requests a server may send, ping with an empty result and anything else with method not found
func (c *Client) answer(req rpcMessage) {
	reply := rpcMessage{ID: req.ID}
	if req.Method == "ping" {
		reply.Result = json.RawMessage(`{}`)
	} else {
		reply.Error = &Error{Code: -32601, Message: "method not found: " + req.Method}
	}
	msg, err := encode(reply, nil)
	if err != nil {
		return
	}
	c.t.Send(context.Background(), msg)
}

func (c *Client) notified(msg rpcMessage) {
	c.mu.Lock()
	onTools, onLog := c.onTools, c.onLog
	c.mu.Unlock()
	switch msg.Method {
	case "notifications/tools/list_changed":
		if onTools != nil {
			onTools()
		}
	case "notifications/message":
		if onLog == nil {
			return
		}
		var p struct {
			Level  string          `json:"level"`
			Logger string          `json:"logger"`
			Data   json.RawMessage `json:"data"`
		}
		if json.Unmarshal(msg.Params, &p) == nil {
			onLog(p.Level, p.Logger, p.Data)
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
