package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Gap allowed between SSE events before the legacy transport declares the stream dead
const sseReadTimeout = 300 * time.Second

// SSETransport speaks the 2024-11-05 HTTP with SSE transport, a GET stream plus POSTs to the announced endpoint
type SSETransport struct {
	oc        *ownedClient
	streamURL string
	endpoint  string
	headers   map[string]string
	recv      chan *Message
	done      chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
	err       error
	body      io.ReadCloser
	ready     chan struct{}
	readyErr  error
}

// StartSSE opens the event stream and waits for the endpoint announcement within connectTimeout
func StartSSE(ctx context.Context, opts HTTPOptions, connectTimeout time.Duration) (*SSETransport, error) {
	oc, err := newOwnedClient(opts)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{}
	for k, v := range opts.Headers {
		headers[k] = v
	}
	t := &SSETransport{oc: oc, streamURL: opts.URL, headers: headers, recv: make(chan *Message, 64),
		done: make(chan struct{}), ready: make(chan struct{})}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, opts.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-store")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := t.doAuth(ctx, req, nil)
	if err != nil {
		return nil, err
	}
	if !is2xx(resp) {
		raw, _ := readCapped(resp.Body, 64*1024, opts.URL)
		resp.Body.Close()
		oc.rejected.record(resp, raw)
		rej := oc.rejected.snapshot()
		return nil, &HTTPStatusError{Status: resp.StatusCode, Method: http.MethodGet, URL: opts.URL, Body: rej.Body}
	}
	if ct := contentTypeBase(resp); ct != "text/event-stream" {
		drainClose(resp.Body)
		return nil, fmt.Errorf("SSE endpoint answered with Content-Type %q, not text/event-stream", ct)
	}
	t.body = resp.Body
	go t.pump(resp.Body)
	select {
	case <-t.ready:
		if t.readyErr != nil {
			t.Close()
			return nil, t.readyErr
		}
		return t, nil
	case <-time.After(connectTimeout):
		t.Close()
		return nil, errors.New("SSE server did not announce its endpoint within the connect timeout")
	case <-ctx.Done():
		t.Close()
		return nil, ctx.Err()
	}
}

// doAuth sends req with the authenticator, retrying once after a handled 401
func (t *SSETransport) doAuth(ctx context.Context, req *http.Request, body []byte) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if body != nil {
			req.Body = io.NopCloser(bytes.NewReader(body))
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
			peek, _ := readCapped(resp.Body, 64*1024, req.URL.String())
			resp.Body.Close()
			t.oc.rejected.record(resp, peek)
			resp.Body = io.NopCloser(bytes.NewReader(peek))
			retry, aerr := t.oc.opts.Auth.HandleUnauthorized(ctx, req, resp)
			if aerr != nil {
				return nil, aerr
			}
			if retry {
				req = req.Clone(ctx)
				continue
			}
		}
		return resp, nil
	}
	return nil, errors.New("authentication retry exhausted")
}

// pump reads events, publishing the endpoint first and messages afterwards under a read watchdog
func (t *SSETransport) pump(body io.ReadCloser) {
	defer func() {
		select {
		case <-t.done:
		default:
			t.setErr(errors.New("Connection closed"))
		}
		close(t.recv)
	}()
	reader := NewSSEReader(body, MaxHTTPBodyBytes, t.streamURL)
	watchdog := time.AfterFunc(sseReadTimeout, func() {
		t.setErr(errors.New("SSE stream idle past the read timeout"))
		body.Close()
	})
	defer watchdog.Stop()
	announced := false
	for {
		ev, err := reader.Next()
		if err != nil {
			if !announced {
				t.readyErr = fmt.Errorf("SSE stream ended before the endpoint event: %w", err)
				close(t.ready)
			} else if err != io.EOF {
				t.setErr(err)
			}
			return
		}
		watchdog.Reset(sseReadTimeout)
		switch ev.Event {
		case "endpoint":
			resolved, rerr := resolveEndpoint(t.streamURL, ev.Data)
			if rerr != nil {
				t.readyErr = rerr
				if !announced {
					close(t.ready)
				}
				return
			}
			t.mu.Lock()
			t.endpoint = resolved
			t.mu.Unlock()
			if !announced {
				announced = true
				close(t.ready)
			}
		case "message", "":
			if strings.TrimSpace(ev.Data) == "" {
				continue
			}
			msgs, derr := DecodeMessages([]byte(ev.Data))
			if derr != nil {
				continue
			}
			for _, m := range msgs {
				select {
				case t.recv <- m:
				case <-t.done:
					return
				}
			}
		}
	}
}

// Send posts one message to the announced endpoint
func (t *SSETransport) Send(ctx context.Context, msg *Message) error {
	select {
	case <-t.done:
		return errors.New("transport is closed")
	default:
	}
	t.mu.Lock()
	endpoint := t.endpoint
	t.mu.Unlock()
	if endpoint == "" {
		return errors.New("SSE endpoint not announced yet")
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	resp, err := t.doAuth(ctx, req, body)
	if err != nil {
		return err
	}
	raw, _ := readCapped(resp.Body, 64*1024, endpoint)
	resp.Body.Close()
	if !is2xx(resp) {
		t.oc.rejected.record(resp, raw)
		rej := t.oc.rejected.snapshot()
		return &HTTPStatusError{Status: resp.StatusCode, Method: http.MethodPost, URL: endpoint, Body: rej.Body}
	}
	return nil
}

// setErr records the first terminal error
func (t *SSETransport) setErr(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err == nil {
		t.err = err
	}
}

// Receive returns the inbound message channel
func (t *SSETransport) Receive() <-chan *Message { return t.recv }

// Err returns the terminal transport error
func (t *SSETransport) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// Label names the transport
func (t *SSETransport) Label() string { return "SSE" }

// Rejection returns the last 4xx or 5xx the owned client saw
func (t *SSETransport) Rejection() HTTPRejection { return t.oc.rejected.snapshot() }

// Close ends the stream and releases the client
func (t *SSETransport) Close() error {
	t.closeOnce.Do(func() {
		close(t.done)
		if t.body != nil {
			t.body.Close()
		}
		t.oc.client.CloseIdleConnections()
	})
	return nil
}
