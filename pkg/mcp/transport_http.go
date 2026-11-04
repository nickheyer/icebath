package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// HTTPStatusError is a non 2xx answer the server gave to a transport request
type HTTPStatusError struct {
	Status int
	Method string
	URL    string
	Body   string
}

// Error renders the status in the SDK's opaque wording plus the recorded detail
func (e *HTTPStatusError) Error() string {
	detail := fmt.Sprintf("HTTP %d from %s %s", e.Status, e.Method, e.URL)
	if e.Body != "" {
		detail += ": " + e.Body
	}
	return fmt.Sprintf("Server returned an error response (%s)", detail)
}

// ErrSessionExpired marks a 404 the server gave for a known session id
var ErrSessionExpired = errors.New("session expired: the server no longer knows this Mcp-Session-Id (session not found)")

// StreamableHTTPTransport speaks the Streamable HTTP transport with session ids and SSE answers
type StreamableHTTPTransport struct {
	oc        *ownedClient
	endpoint  string
	headers   map[string]string
	recv      chan *Message
	done      chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
	sessionID string
	protocol  string
	err       error
	streams   sync.WaitGroup
	getOnce   sync.Once
	lastEvent string
	label     string
}

// StartStreamableHTTP builds the transport, no request is sent until the first Send
func StartStreamableHTTP(opts HTTPOptions) (*StreamableHTTPTransport, error) {
	oc, err := newOwnedClient(opts)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{}
	for k, v := range opts.Headers {
		headers[k] = v
	}
	t := &StreamableHTTPTransport{oc: oc, endpoint: opts.URL, headers: headers, recv: make(chan *Message, 64),
		done: make(chan struct{}), label: "HTTP"}
	if v, ok := headerLookup(headers, "mcp-protocol-version"); ok {
		t.protocol = v
	} else {
		t.protocol = LatestHandshakeVersion
	}
	return t, nil
}

// headerLookup finds a header value by case insensitive name
func headerLookup(headers map[string]string, name string) (string, bool) {
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}

// Rejection returns the last 4xx or 5xx the owned client saw this connection
func (t *StreamableHTTPTransport) Rejection() HTTPRejection { return t.oc.rejected.snapshot() }

// SetProtocolVersion pins the MCP-Protocol-Version header for every later request
func (t *StreamableHTTPTransport) SetProtocolVersion(v string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if v != "" {
		t.protocol = v
	}
}

// SessionID returns the server assigned session id, empty when the server is stateless
func (t *StreamableHTTPTransport) SessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

// applyHeaders stamps configured headers, the session id and the protocol version onto req
func (t *StreamableHTTPTransport) applyHeaders(req *http.Request) {
	for k, v := range t.headers {
		if strings.EqualFold(k, "mcp-protocol-version") {
			continue
		}
		req.Header.Set(k, v)
	}
	t.mu.Lock()
	sid, proto := t.sessionID, t.protocol
	t.mu.Unlock()
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	if proto != "" {
		req.Header.Set("MCP-Protocol-Version", proto)
	}
}

// send performs one request with authentication, retrying once after a handled 401
func (t *StreamableHTTPTransport) send(ctx context.Context, method string, body []byte, accept string) (*http.Response, error) {
	build := func() (*http.Request, error) {
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
		t.applyHeaders(req)
		return req, nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := build()
		if err != nil {
			return nil, err
		}
		if t.oc.opts.Auth != nil {
			if err := t.oc.opts.Auth.Prepare(ctx, req); err != nil {
				return nil, err
			}
		}
		resp, err := t.oc.client.Do(req)
		if err != nil {
			return nil, err
		}
		if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && t.oc.opts.Auth != nil && attempt == 0 {
			peek, _ := readCapped(resp.Body, 64*1024, t.endpoint)
			resp.Body.Close()
			t.oc.rejected.record(resp, peek)
			resp.Body = io.NopCloser(bytes.NewReader(peek))
			retry, aerr := t.oc.opts.Auth.HandleUnauthorized(ctx, req, resp)
			if aerr != nil {
				return nil, aerr
			}
			if retry {
				continue
			}
			return resp, nil
		}
		return resp, nil
	}
	return nil, errors.New("authentication retry exhausted")
}

// Send posts one message, delivering any JSON or SSE answer onto the receive channel
func (t *StreamableHTTPTransport) Send(ctx context.Context, msg *Message) error {
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
		drainClose(resp.Body)
		return nil
	}
	if !is2xx(resp) {
		raw, _ := readCapped(resp.Body, 64*1024, t.endpoint)
		resp.Body.Close()
		t.oc.rejected.record(resp, raw)
		if resp.StatusCode == http.StatusNotFound && t.SessionID() != "" {
			return ErrSessionExpired
		}
		rej := t.oc.rejected.snapshot()
		return &HTTPStatusError{Status: resp.StatusCode, Method: http.MethodPost, URL: t.endpoint, Body: rej.Body}
	}
	if msg.Method == methodInitialize {
		if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
			t.mu.Lock()
			t.sessionID = sid
			t.mu.Unlock()
		}
	}
	switch ct := contentTypeBase(resp); ct {
	case "application/json":
		raw, err := readCapped(resp.Body, MaxHTTPBodyBytes, t.endpoint)
		resp.Body.Close()
		if err != nil {
			return err
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			return nil
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
		go t.pumpStream(resp.Body, msg.ID, false)
		return nil
	default:
		drainClose(resp.Body)
		if msg.IsNotification() {
			return nil
		}
		return fmt.Errorf("MCP endpoint answered with Content-Type %q, not an MCP response", ct)
	}
}

// deliver hands one inbound message to the session unless the transport closed
func (t *StreamableHTTPTransport) deliver(m *Message) {
	select {
	case t.recv <- m:
	case <-t.done:
	}
}

// pumpStream reads SSE events into messages, ending at EOF or after the awaited response on a POST stream
func (t *StreamableHTTPTransport) pumpStream(body io.ReadCloser, awaiting json.RawMessage, standalone bool) {
	defer t.streams.Done()
	defer body.Close()
	stop := make(chan struct{})
	go func() {
		select {
		case <-t.done:
			body.Close()
		case <-stop:
		}
	}()
	defer close(stop)
	reader := NewSSEReader(body, MaxHTTPBodyBytes, t.endpoint)
	for {
		ev, err := reader.Next()
		if err != nil {
			if err != io.EOF && standalone {
				t.setErr(err)
			} else if err != io.EOF && errors.Is(err, ErrBodyTooLarge) {
				t.setErr(err)
			}
			return
		}
		if ev.ID != "" {
			t.mu.Lock()
			t.lastEvent = ev.ID
			t.mu.Unlock()
		}
		if ev.Event != "" && ev.Event != "message" {
			continue
		}
		if strings.TrimSpace(ev.Data) == "" {
			continue
		}
		msgs, derr := DecodeMessages([]byte(ev.Data))
		if derr != nil {
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

// OpenStandaloneStream starts the optional GET event stream for server initiated messages
func (t *StreamableHTTPTransport) OpenStandaloneStream(ctx context.Context) {
	t.getOnce.Do(func() {
		resp, err := t.send(ctx, http.MethodGet, nil, "text/event-stream")
		if err != nil {
			return
		}
		if !is2xx(resp) || contentTypeBase(resp) != "text/event-stream" {
			drainClose(resp.Body)
			return
		}
		t.streams.Add(1)
		go t.pumpStream(resp.Body, nil, true)
	})
}

// setErr records the first terminal error
func (t *StreamableHTTPTransport) setErr(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err == nil {
		t.err = err
	}
}

// Receive returns the inbound message channel
func (t *StreamableHTTPTransport) Receive() <-chan *Message { return t.recv }

// Err returns the terminal transport error
func (t *StreamableHTTPTransport) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// Label names the transport
func (t *StreamableHTTPTransport) Label() string { return t.label }

// Close deletes the session, stops every stream and closes the receive channel
func (t *StreamableHTTPTransport) Close() error {
	t.closeOnce.Do(func() {
		close(t.done)
		if sid := t.SessionID(); sid != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.endpoint, nil)
			if err == nil {
				t.applyHeaders(req)
				if t.oc.opts.Auth != nil {
					_ = t.oc.opts.Auth.Prepare(ctx, req)
				}
				if resp, err := t.oc.client.Do(req); err == nil {
					drainClose(resp.Body)
				}
			}
			cancel()
		}
		t.oc.client.CloseIdleConnections()
		go func() {
			t.streams.Wait()
			close(t.recv)
		}()
	})
	return nil
}

// resolveEndpoint resolves a relative endpoint URL against the stream URL enforcing one origin
func resolveEndpoint(base string, endpoint string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	e, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", err
	}
	resolved := b.ResolveReference(e)
	if originOf(resolved) != originOf(b) {
		return "", fmt.Errorf("endpoint origin %s does not match connection origin %s", resolved.Host, b.Host)
	}
	return resolved.String(), nil
}
