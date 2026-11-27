package mcp

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http/httpproxy"
)

// Adds credentials to requests and reacts to a refusal
type Authenticator interface {
	// Adds credentials to the request, getting them first when none are held
	Prepare(ctx context.Context, req *http.Request) error
	// Reacts to a 401 or 403 answer, true when the request should be sent again
	HandleUnauthorized(ctx context.Context, req *http.Request, resp *http.Response) (bool, error)
}

// How one remote server is reached
type HTTPOptions struct {
	URL     string
	Headers map[string]string
	// Names of the headers the server's settings set, lower cased, dropped on a redirect to another origin when strict
	Configured map[string]bool
	Strict     bool
	// TLS verification: true, false, or the path of a CA bundle; nil verifies
	SSLVerify any
	// Client certificate: a path to a PEM holding both, a path with ClientKey, or a list of cert, key and passphrase
	ClientCert any
	ClientKey  any
	// Longest a connection may take to open, and an answer to start
	ConnectTimeout time.Duration
	ReadTimeout    time.Duration
	Auth           Authenticator
	// Server the options are of, named in errors
	Server string
}

// Last 4xx or 5xx a client saw, kept to explain an opaque failure
type HTTPRejection struct {
	Status int
	Method string
	URL    string
	Body   string
}

func (r HTTPRejection) Describe() string {
	if r.Status == 0 {
		return ""
	}
	detail := fmt.Sprintf("HTTP %d from %s %s", r.Status, r.Method, r.URL)
	if r.Body != "" {
		detail += ": " + r.Body
	}
	return detail
}

type rejections struct {
	mu   sync.Mutex
	last HTTPRejection
}

func (r *rejections) record(resp *http.Response, body []byte) {
	if resp == nil || resp.StatusCode < 400 {
		return
	}
	head := strings.Join(strings.Fields(string(body)), " ")
	head = head[:min(len(head), 300)]
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = HTTPRejection{Status: resp.StatusCode, Method: resp.Request.Method, URL: resp.Request.URL.String(), Body: head}
}

func (r *rejections) snapshot() HTTPRejection {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// Client of one remote server: its TLS, proxy, credential boundary on redirects and last refusal
type Client struct {
	*http.Client
	opts     HTTPOptions
	origin   origin
	rejected *rejections
}

type origin struct{ scheme, host, port string }

func originOf(u *url.URL) origin {
	port := u.Port()
	if port == "" {
		port = "80"
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		}
	}
	return origin{scheme: strings.ToLower(u.Scheme), host: strings.ToLower(u.Hostname()), port: port}
}

func NewClient(opts HTTPOptions) (*Client, error) {
	target, err := url.Parse(opts.URL)
	if err != nil {
		return nil, err
	}
	tlsConfig, err := tlsConfig(opts.Server, opts.SSLVerify, opts.ClientCert, opts.ClientKey)
	if err != nil {
		return nil, err
	}
	connect := cmp.Or(opts.ConnectTimeout, 30*time.Second)
	transport := &http.Transport{
		Proxy:                 proxy(),
		DialContext:           (&net.Dialer{Timeout: connect, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   connect,
		ResponseHeaderTimeout: cmp.Or(opts.ReadTimeout, 300*time.Second),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		IdleConnTimeout:       90 * time.Second,
	}
	c := &Client{opts: opts, origin: originOf(target), rejected: &rejections{}}
	c.Client = &http.Client{Transport: capped{inner: transport}, CheckRedirect: c.redirect}
	return c, nil
}

// Proxy of each request from the environment, ALL_PROXY standing in for the scheme's own, NO_PROXY and this machine going direct
func proxy() func(*http.Request) (*url.URL, error) {
	env := func(names ...string) string {
		for _, name := range names {
			if v := os.Getenv(name); v != "" {
				return v
			}
		}
		return ""
	}
	all := env("all_proxy", "ALL_PROXY")
	config := httpproxy.Config{
		HTTPProxy:  cmp.Or(env("http_proxy", "HTTP_PROXY"), all),
		HTTPSProxy: cmp.Or(env("https_proxy", "HTTPS_PROXY"), all),
		NoProxy:    env("no_proxy", "NO_PROXY"),
		CGI:        os.Getenv("REQUEST_METHOD") != "",
	}
	find := config.ProxyFunc()
	return func(req *http.Request) (*url.URL, error) { return find(req.URL) }
}

// Drops Authorization, and when strict every configured header, on a hop to another origin
func (c *Client) redirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if originOf(req.URL) != c.origin {
		req.Header.Del("Authorization")
		if c.opts.Strict {
			for name := range c.opts.Configured {
				req.Header.Del(name)
			}
		}
	}
	return nil
}

// Last 4xx or 5xx this client saw
func (c *Client) Rejection() HTTPRejection { return c.rejected.snapshot() }

func drain(body io.ReadCloser) {
	if body == nil {
		return
	}
	io.Copy(io.Discard, io.LimitReader(body, 1<<20))
	body.Close()
}

// Content type of an answer, lowered and without parameters
func ContentType(resp *http.Response) string { return BaseMime(resp.Header.Get("Content-Type")) }

func success(resp *http.Response) bool { return resp.StatusCode >= 200 && resp.StatusCode < 300 }

// Content types a real MCP endpoint answers with
var mcpTypes = map[string]bool{"application/json": true, "text/event-stream": true}

// Non 2xx answer a server gave a transport request
type HTTPStatusError struct {
	Status int
	Method string
	URL    string
	Body   string
}

func (e *HTTPStatusError) Error() string {
	detail := fmt.Sprintf("HTTP %d from %s %s", e.Status, e.Method, e.URL)
	if e.Body != "" {
		detail += ": " + e.Body
	}
	return "Server returned an error response (" + detail + ")"
}

// A 404 the server gave for a session id it knew
var ErrSessionExpired = errors.New("session expired: the server no longer knows this Mcp-Session-Id (session not found)")

// Streamable HTTP transport: one POST per message, answers as JSON or a stream of events, an optional GET stream for server requests
type StreamableHTTP struct {
	client   *Client
	endpoint string
	headers  map[string]string
	recv     chan *Message
	done     chan struct{}
	once     sync.Once
	streams  sync.WaitGroup
	get      sync.Once

	mu        sync.Mutex
	sessionID string
	protocol  string
	err       error
}

// The transport; nothing is sent before the first message
func StartStreamableHTTP(opts HTTPOptions) (*StreamableHTTP, error) {
	client, err := NewClient(opts)
	if err != nil {
		return nil, err
	}
	t := &StreamableHTTP{client: client, endpoint: opts.URL, headers: map[string]string{}, recv: make(chan *Message, 64), done: make(chan struct{}),
		protocol: HandshakeVersion}
	for k, v := range opts.Headers {
		if strings.EqualFold(k, "mcp-protocol-version") {
			t.protocol = v
			continue
		}
		t.headers[k] = v
	}
	return t, nil
}

func (t *StreamableHTTP) Rejection() HTTPRejection { return t.client.Rejection() }

// Pins the MCP-Protocol-Version header of every later request
func (t *StreamableHTTP) SetProtocolVersion(v string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if v != "" {
		t.protocol = v
	}
}

// Session id the server assigned, empty for a stateless server
func (t *StreamableHTTP) SessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

func (t *StreamableHTTP) stamp(req *http.Request) {
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	t.mu.Lock()
	sid, protocol := t.sessionID, t.protocol
	t.mu.Unlock()
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	if protocol != "" {
		req.Header.Set("MCP-Protocol-Version", protocol)
	}
}

// One request with credentials, sent again once after a refusal the authenticator handled
func (t *StreamableHTTP) send(ctx context.Context, method string, body []byte, accept string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, t.endpoint, reader)
		if err != nil {
			return nil, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", accept)
		t.stamp(req)
		auth := t.client.opts.Auth
		if auth != nil {
			if err := auth.Prepare(ctx, req); err != nil {
				return nil, err
			}
		}
		resp, err := t.client.Do(req)
		if err != nil {
			return nil, err
		}
		if auth == nil || attempt > 0 || resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
			return resp, nil
		}
		peek, _ := readCapped(resp.Body, 64<<10, t.endpoint)
		resp.Body.Close()
		t.client.rejected.record(resp, peek)
		resp.Body = io.NopCloser(bytes.NewReader(peek))
		retry, err := auth.HandleUnauthorized(ctx, req, resp)
		if err != nil {
			return nil, err
		}
		if !retry {
			return resp, nil
		}
	}
}

func (t *StreamableHTTP) Send(ctx context.Context, msg *Message) error {
	select {
	case <-t.done:
		return errors.New("transport is closed")
	default:
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	resp, err := t.send(ctx, http.MethodPost, body, "application/json, text/event-stream")
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		drain(resp.Body)
		return nil
	}
	if !success(resp) {
		raw, _ := readCapped(resp.Body, 64<<10, t.endpoint)
		resp.Body.Close()
		t.client.rejected.record(resp, raw)
		if resp.StatusCode == http.StatusNotFound && t.SessionID() != "" {
			return ErrSessionExpired
		}
		return &HTTPStatusError{Status: resp.StatusCode, Method: http.MethodPost, URL: t.endpoint, Body: t.client.Rejection().Body}
	}
	if msg.Method == MethodInitialize {
		if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
			t.mu.Lock()
			t.sessionID = sid
			t.mu.Unlock()
		}
	}
	switch kind := ContentType(resp); kind {
	case "application/json":
		raw, err := readCapped(resp.Body, MaxBodyBytes, t.endpoint)
		resp.Body.Close()
		if err != nil || len(bytes.TrimSpace(raw)) == 0 {
			return err
		}
		msgs, err := DecodeMessages(raw)
		if err != nil {
			return err
		}
		for _, m := range msgs {
			t.deliver(m)
		}
		return nil
	case "text/event-stream":
		t.streams.Add(1)
		go t.pump(resp.Body, msg.ID, false)
		return nil
	default:
		drain(resp.Body)
		if msg.IsNotification() {
			return nil
		}
		return fmt.Errorf("MCP endpoint answered with Content-Type %q, not an MCP response", kind)
	}
}

func (t *StreamableHTTP) deliver(m *Message) {
	select {
	case t.recv <- m:
	case <-t.done:
	}
}

// Reads events into messages until the stream ends, or on a POST stream until the awaited answer came
func (t *StreamableHTTP) pump(body io.ReadCloser, awaiting json.RawMessage, standalone bool) {
	defer t.streams.Done()
	defer body.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-t.done:
			body.Close()
		case <-stop:
		}
	}()
	reader := NewSSEReader(body, MaxBodyBytes, t.endpoint)
	for {
		event, err := reader.Next()
		if err != nil {
			if err != io.EOF && (standalone || errors.Is(err, ErrBodyTooLarge)) {
				t.setErr(err)
			}
			return
		}
		if event.Event != "" && event.Event != "message" || strings.TrimSpace(event.Data) == "" {
			continue
		}
		msgs, err := DecodeMessages([]byte(event.Data))
		if err != nil {
			continue
		}
		for _, m := range msgs {
			t.deliver(m)
			if len(awaiting) > 0 && m.IsResponse() && IDKey(m.ID) == IDKey(awaiting) {
				return
			}
		}
	}
}

// Opens the optional GET stream the server sends its own requests on
func (t *StreamableHTTP) OpenStream(ctx context.Context) {
	t.get.Do(func() {
		resp, err := t.send(ctx, http.MethodGet, nil, "text/event-stream")
		if err != nil {
			return
		}
		if !success(resp) || ContentType(resp) != "text/event-stream" {
			drain(resp.Body)
			return
		}
		t.streams.Add(1)
		go t.pump(resp.Body, nil, true)
	})
}

func (t *StreamableHTTP) setErr(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err == nil {
		t.err = err
	}
}

func (t *StreamableHTTP) Receive() <-chan *Message { return t.recv }

func (t *StreamableHTTP) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

func (t *StreamableHTTP) Label() string { return "HTTP" }

// Ends the server's session, stops every stream and closes the receive channel
func (t *StreamableHTTP) Close() error {
	t.once.Do(func() {
		close(t.done)
		if sid := t.SessionID(); sid != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.endpoint, nil); err == nil {
				t.stamp(req)
				if t.client.opts.Auth != nil {
					t.client.opts.Auth.Prepare(ctx, req)
				}
				if resp, err := t.client.Do(req); err == nil {
					drain(resp.Body)
				}
			}
			cancel()
		}
		t.client.CloseIdleConnections()
		go func() {
			t.streams.Wait()
			close(t.recv)
		}()
	})
	return nil
}
