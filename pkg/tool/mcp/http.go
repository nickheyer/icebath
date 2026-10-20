package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/sse"
)

// Server reached over the streamable HTTP transport at one endpoint URL
type HTTP struct {
	URL string
	// Static headers such as Authorization sent on every request
	Header http.Header
	// Client with a thirty second timeout when nil
	Client *http.Client
}

type httpConn struct {
	url     string
	header  http.Header
	client  *http.Client
	ctx     context.Context
	cancel  context.CancelFunc
	in      chan []byte
	mu      sync.Mutex
	session string
	version string
	initID  string
	wg      sync.WaitGroup
	once    sync.Once
	gate    sync.RWMutex
	closed  bool
}

func (h HTTP) Dial(ctx context.Context) (Transport, error) {
	if h.URL == "" {
		return nil, errors.New("mcp: http url is required")
	}
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	cctx, cancel := context.WithCancel(context.Background())
	return &httpConn{url: h.URL, header: h.Header, client: client, ctx: cctx, cancel: cancel, in: make(chan []byte, 16)}, nil
}

func (c *httpConn) Receive() <-chan []byte { return c.in }

// Posts the message and feeds whatever the server answers, JSON or an event stream, to the receiver
func (c *httpConn) Send(ctx context.Context, msg []byte) error {
	var out rpcMessage
	if json.Unmarshal(msg, &out) == nil && out.Method == "initialize" {
		c.mu.Lock()
		c.initID = string(out.ID)
		c.mu.Unlock()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(msg))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	c.decorate(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		c.session = sid
		c.mu.Unlock()
	}
	switch {
	case resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent:
		return nil
	case resp.StatusCode == http.StatusNotFound && c.sessionID() != "":
		return errors.New("mcp: server session expired")
	case resp.StatusCode/100 != 2:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("mcp: http %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	kind, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch kind {
	case "application/json":
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		c.deliver(body)
	case "text/event-stream":
		for ev, err := range sse.Read(resp.Body) {
			if err != nil {
				return err
			}
			c.deliver([]byte(ev.Data))
		}
	default:
		return fmt.Errorf("mcp: unexpected content type %q", kind)
	}
	return nil
}

// Pushes a message inward, learning the negotiated version from the initialize response
func (c *httpConn) deliver(raw []byte) {
	c.gate.RLock()
	defer c.gate.RUnlock()
	if c.closed {
		return
	}
	for _, msg := range split(raw) {
		c.mu.Lock()
		initID := c.initID
		c.mu.Unlock()
		if initID != "" && string(msg.ID) == initID && msg.Method == "" && len(msg.Result) > 0 {
			var res initializeResult
			if json.Unmarshal(msg.Result, &res) == nil && res.ProtocolVersion != "" {
				c.mu.Lock()
				c.version = res.ProtocolVersion
				c.initID = ""
				c.mu.Unlock()
				c.wg.Add(1)
				go c.listen()
			}
		}
		encoded, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		select {
		case c.in <- encoded:
		case <-c.ctx.Done():
			return
		}
	}
}

// Opens the server to client event stream, which a server may decline with 405
func (c *httpConn) listen() {
	defer c.wg.Done()
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	c.decorate(req)
	stream := *c.client
	stream.Timeout = 0
	resp, err := stream.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	for ev, err := range sse.Read(resp.Body) {
		if err != nil {
			return
		}
		c.deliver([]byte(ev.Data))
	}
}

func (c *httpConn) sessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session
}

// Adds the static headers, the session id and the negotiated protocol version
func (c *httpConn) decorate(req *http.Request) {
	for k, vs := range c.header {
		req.Header[k] = append([]string(nil), vs...)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	if c.version != "" {
		req.Header.Set("MCP-Protocol-Version", c.version)
	}
}

// Ends the session with a DELETE, stops the event stream and closes the receiver
func (c *httpConn) Close() error {
	c.once.Do(func() {
		if sid := c.sessionID(); sid != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.url, nil); err == nil {
				c.decorate(req)
				if resp, err := c.client.Do(req); err == nil {
					resp.Body.Close()
				}
			}
			cancel()
		}
		c.cancel()
		c.wg.Wait()
		c.gate.Lock()
		c.closed = true
		close(c.in)
		c.gate.Unlock()
	})
	return nil
}
