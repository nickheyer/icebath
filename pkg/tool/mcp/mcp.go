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

// Answers a sampling or elicitation request from the server with the raw result
type Handler func(ctx context.Context, params json.RawMessage) (json.RawMessage, error)

// Progress a server reports for one request
type Progress struct {
	Token    string
	Progress float64
	Total    float64
	Message  string
}

// What the client declares and answers for a server
type Options struct {
	Info Info
	// Roots served to the server, declaring the roots capability when non empty
	Roots []Root
	// Answers sampling/createMessage requests, declaring the sampling capability when set
	Sampler Handler
	// Answers elicitation/create requests, declaring the elicitation capability when set
	Elicitor Handler
}

// What the server said about itself during initialization
type ServerInfo struct {
	Name               string
	Version            string
	ProtocolVersion    string
	Instructions       string
	Tools              bool
	ToolsChange        bool
	Resources          bool
	ResourcesChange    bool
	ResourcesSubscribe bool
	Prompts            bool
	PromptsChange      bool
	Logging            bool
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
		Resources *struct {
			ListChanged bool `json:"listChanged"`
			Subscribe   bool `json:"subscribe"`
		} `json:"resources"`
		Prompts *struct {
			ListChanged bool `json:"listChanged"`
		} `json:"prompts"`
		Logging *struct{} `json:"logging"`
	} `json:"capabilities"`
	ServerInfo   Info   `json:"serverInfo"`
	Instructions string `json:"instructions"`
}

// Connected server with its request multiplexer
type Client struct {
	name       string
	t          Transport
	opts       Options
	mu         sync.Mutex
	next       int64
	pending    map[string]chan rpcMessage
	progress   map[string]context.Context
	closed     chan struct{}
	closeOnce  sync.Once
	err        error
	server     ServerInfo
	notes      chan rpcMessage
	notesDone  chan struct{}
	onTools    func()
	onLog      func(level, logger string, data json.RawMessage)
	onRes      func()
	onResUpd   func(uri string)
	onPrompts  func()
	onProgress func(Progress)
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
	raw, err := c.Call(ctx, "initialize", initializeParams{ProtocolVersion: Version, Capabilities: c.capabilities(), ClientInfo: o.Info})
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
	c.server = ServerInfo{Name: res.ServerInfo.Name, Version: res.ServerInfo.Version, ProtocolVersion: res.ProtocolVersion, Instructions: res.Instructions}
	if res.Capabilities.Tools != nil {
		c.server.Tools, c.server.ToolsChange = true, res.Capabilities.Tools.ListChanged
	}
	if res.Capabilities.Resources != nil {
		c.server.Resources, c.server.ResourcesChange, c.server.ResourcesSubscribe = true, res.Capabilities.Resources.ListChanged, res.Capabilities.Resources.Subscribe
	}
	if res.Capabilities.Prompts != nil {
		c.server.Prompts, c.server.PromptsChange = true, res.Capabilities.Prompts.ListChanged
	}
	c.server.Logging = res.Capabilities.Logging != nil
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

// Capabilities declared to the server from the handlers the options carry
func (c *Client) capabilities() map[string]any {
	caps := map[string]any{}
	if len(c.opts.Roots) > 0 {
		caps["roots"] = map[string]any{"listChanged": false}
	}
	if c.opts.Sampler != nil {
		caps["sampling"] = map[string]any{}
	}
	if c.opts.Elicitor != nil {
		caps["elicitation"] = map[string]any{}
	}
	return caps
}

// Label used in tool names
func (c *Client) Name() string { return c.name }

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

// Registers the callback run for every log notification from the server
func (c *Client) OnLog(fn func(level, logger string, data json.RawMessage)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onLog = fn
}

// Registers the callback run when the server announces a changed resource list
func (c *Client) OnResourcesChanged(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onRes = fn
}

// Registers the callback run when a subscribed resource changes
func (c *Client) OnResourceUpdated(fn func(uri string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onResUpd = fn
}

// Registers the callback run when the server announces a changed prompt list
func (c *Client) OnPromptsChanged(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onPrompts = fn
}

// Registers the callback run for every progress notification
func (c *Client) OnProgress(fn func(Progress)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onProgress = fn
}

// Sends a request and waits for its result, failing on an error response or a closed transport
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return c.call(ctx, method, params, "")
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
	return c.call(ctx, method, params, token)
}

func (c *Client) call(ctx context.Context, method string, params any, token string) (json.RawMessage, error) {
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

// Checks the server is alive
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.Call(ctx, "ping", nil)
	return err
}

// Asks the server to send log notifications at level and above
func (c *Client) SetLogLevel(ctx context.Context, level string) error {
	_, err := c.Call(ctx, "logging/setLevel", map[string]string{"level": level})
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

// Answers the requests a server may send using the configured handlers
func (c *Client) answer(req rpcMessage) {
	reply := rpcMessage{ID: req.ID}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
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
	case "sampling/createMessage":
		reply = c.handle(reply, c.opts.Sampler, ctx, req)
	case "elicitation/create":
		reply = c.handle(reply, c.opts.Elicitor, ctx, req)
	default:
		reply.Error = &Error{Code: -32601, Message: "method not found: " + req.Method}
	}
	msg, err := encode(reply, nil)
	if err != nil {
		return
	}
	c.t.Send(context.Background(), msg)
}

// Runs a handler for a server request, mapping a missing handler or a failure to a JSON-RPC error
func (c *Client) handle(reply rpcMessage, h Handler, ctx context.Context, req rpcMessage) rpcMessage {
	if h == nil {
		reply.Error = &Error{Code: -32601, Message: "method not found: " + req.Method}
		return reply
	}
	res, err := h(ctx, req.Params)
	if err != nil {
		var rpcErr *Error
		if errors.As(err, &rpcErr) {
			reply.Error = rpcErr
		} else {
			reply.Error = &Error{Code: -32603, Message: err.Error()}
		}
		return reply
	}
	if len(res) == 0 {
		res = json.RawMessage(`{}`)
	}
	reply.Result = res
	return reply
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
	onTools, onLog, onRes, onResUpd, onPrompts, onProgress := c.onTools, c.onLog, c.onRes, c.onResUpd, c.onPrompts, c.onProgress
	c.mu.Unlock()
	switch msg.Method {
	case "notifications/tools/list_changed":
		if onTools != nil {
			onTools()
		}
	case "notifications/resources/list_changed":
		if onRes != nil {
			onRes()
		}
	case "notifications/prompts/list_changed":
		if onPrompts != nil {
			onPrompts()
		}
	case "notifications/resources/updated":
		var p struct {
			URI string `json:"uri"`
		}
		if onResUpd != nil && json.Unmarshal(msg.Params, &p) == nil {
			onResUpd(p.URI)
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
		prog := Progress{Token: token, Progress: p.Progress, Total: p.Total, Message: p.Message}
		c.mu.Lock()
		ctx := c.progress[token]
		c.mu.Unlock()
		if ctx != nil {
			tool.Report(ctx, tool.Progress{Text: p.Message, Done: int(p.Progress), Total: int(p.Total)})
		}
		if onProgress != nil {
			onProgress(prog)
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
