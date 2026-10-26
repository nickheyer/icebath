package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/sse"
)

// Server started as a child process speaking newline delimited JSON on its standard streams
type Stdio struct {
	Command string
	Args    []string
	// KEY=VALUE pairs layered over the process environment
	Env []string
	Dir string
	// Receives the server standard error, discarded when nil
	Stderr io.Writer
	// Time a server gets to exit after stdin closes before it is killed, two seconds when zero
	Grace time.Duration
}

type stdioConn struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	wmu   sync.Mutex
	in    chan []byte
	done  chan struct{}
	once  sync.Once
	grace time.Duration
	mu    sync.Mutex
	exit  error
	ended bool
}

func (s Stdio) Dial(ctx context.Context) (Transport, error) {
	if s.Command == "" {
		return nil, errors.New("mcp: stdio command is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(s.Command, s.Args...)
	cmd.Dir = s.Dir
	cmd.Env = append(os.Environ(), s.Env...)
	cmd.Stderr = s.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = io.Discard
	}
	proc.Group(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	grace := s.Grace
	if grace <= 0 {
		grace = 2 * time.Second
	}
	c := &stdioConn{cmd: cmd, stdin: stdin, in: make(chan []byte, 16), done: make(chan struct{}), grace: grace}
	go c.pump(stdout)
	return c, nil
}

// Forwards each non empty line of server output until the stream ends, then records the exit
func (c *stdioConn) pump(r io.Reader) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			c.in <- trimmed
		}
		if err != nil {
			break
		}
	}
	exit := c.cmd.Wait()
	c.mu.Lock()
	c.exit, c.ended = exit, true
	c.mu.Unlock()
	close(c.in)
}

// Exit error of the server process once it has ended
func (c *stdioConn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ended {
		return nil
	}
	if c.exit == nil {
		return errors.New("server process exited")
	}
	return c.exit
}

func (c *stdioConn) Send(ctx context.Context, msg []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	select {
	case <-c.done:
		return errors.New("mcp: stdio transport closed")
	default:
	}
	_, err := c.stdin.Write(append(msg, '\n'))
	return err
}

func (c *stdioConn) Receive() <-chan []byte { return c.in }

// Closes stdin, waits for a clean exit within the grace period and kills the process group otherwise
func (c *stdioConn) Close() error {
	c.once.Do(func() {
		close(c.done)
		c.stdin.Close()
		exited := make(chan struct{})
		go func() {
			for range c.in {
			}
			close(exited)
		}()
		select {
		case <-exited:
		case <-time.After(c.grace):
			proc.Kill(c.cmd)
			<-exited
		}
	})
	return nil
}

// Server reached over the streamable HTTP transport at one endpoint URL
type HTTP struct {
	URL string
	// Static headers such as Authorization sent on every request
	Header http.Header
	// Client with connection timeouts and no overall deadline when nil
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
	lastID  string
	wg      sync.WaitGroup
	once    sync.Once
	gate    sync.RWMutex
	closed  bool
	err     error
	listen  bool
}

// Client that bounds connection setup and headers without limiting how long a stream may run
func defaultClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		MaxIdleConns:          8,
		IdleConnTimeout:       90 * time.Second,
	}}
}

func (h HTTP) Dial(ctx context.Context) (Transport, error) {
	if h.URL == "" {
		return nil, errors.New("mcp: http url is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client := h.Client
	if client == nil {
		client = defaultClient()
	}
	cctx, cancel := context.WithCancel(context.Background())
	return &httpConn{url: h.URL, header: h.Header, client: client, ctx: cctx, cancel: cancel, in: make(chan []byte, 16)}, nil
}

func (c *httpConn) Receive() <-chan []byte { return c.in }

// Why the transport stopped, ErrSessionExpired when the server dropped the session
func (c *httpConn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

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
		c.expire()
		return ErrSessionExpired
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

// Marks the session as gone and closes the receiver so the client reports the expiry
func (c *httpConn) expire() {
	c.mu.Lock()
	c.err = ErrSessionExpired
	c.mu.Unlock()
	c.shutdown()
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
				start := !c.listen
				c.listen = true
				c.mu.Unlock()
				if start {
					c.wg.Add(1)
					go c.listenLoop()
				}
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

// Keeps the server to client event stream open, resuming from the last event id with backoff
func (c *httpConn) listenLoop() {
	defer c.wg.Done()
	delay := time.Second
	for {
		status, err := c.listenOnce()
		if c.ctx.Err() != nil || status == http.StatusMethodNotAllowed || status == http.StatusNotFound {
			return
		}
		if err == nil && status == http.StatusOK {
			delay = time.Second
		}
		select {
		case <-time.After(delay):
		case <-c.ctx.Done():
			return
		}
		delay = min(delay*2, 30*time.Second)
	}
}

// Opens one event stream, delivering events until it ends and remembering the last event id
func (c *httpConn) listenOnce() (int, error) {
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "text/event-stream")
	c.decorate(req)
	c.mu.Lock()
	if c.lastID != "" {
		req.Header.Set("Last-Event-ID", c.lastID)
	}
	c.mu.Unlock()
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	for ev, err := range sse.Read(resp.Body) {
		if err != nil {
			return resp.StatusCode, err
		}
		if ev.ID != "" {
			c.mu.Lock()
			c.lastID = ev.ID
			c.mu.Unlock()
		}
		c.deliver([]byte(ev.Data))
	}
	return resp.StatusCode, nil
}

func (c *httpConn) sessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session
}

// Adds the static headers, the session id and the negotiated protocol version
func (c *httpConn) decorate(req *http.Request) {
	for k, vs := range c.header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
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

// Stops the event stream and closes the receiver
func (c *httpConn) shutdown() {
	c.once.Do(func() {
		c.cancel()
		c.wg.Wait()
		c.gate.Lock()
		c.closed = true
		close(c.in)
		c.gate.Unlock()
	})
}

// Ends the session with a DELETE, stops the event stream and closes the receiver
func (c *httpConn) Close() error {
	if sid := c.sessionID(); sid != "" && c.Err() == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.url, nil); err == nil {
			c.decorate(req)
			if resp, err := c.client.Do(req); err == nil {
				resp.Body.Close()
			}
		}
		cancel()
	}
	c.shutdown()
	return nil
}
