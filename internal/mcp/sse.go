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

// Longest the event stream may stay silent before it is taken for dead
const sseIdle = 300 * time.Second

// Legacy SSE transport: one GET event stream for answers, messages posted to the endpoint it announces
type SSE struct {
	client  *Client
	stream  string
	headers map[string]string
	recv    chan *Message
	done    chan struct{}
	once    sync.Once
	body    io.ReadCloser
	ready   chan struct{}
	cancel  context.CancelFunc

	mu       sync.Mutex
	endpoint string
	err      error
	readyErr error
}

// Opens the event stream and waits for its endpoint within the connect timeout
func StartSSE(ctx context.Context, opts HTTPOptions, connectTimeout time.Duration) (*SSE, error) {
	client, err := NewClient(opts)
	if err != nil {
		return nil, err
	}
	t := &SSE{client: client, stream: opts.URL, headers: opts.Headers, recv: make(chan *Message, 64), done: make(chan struct{}), ready: make(chan struct{})}
	// The stream lives as long as the transport, the connect context bounding only its opening
	streamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	t.cancel = cancel
	detach := context.AfterFunc(ctx, cancel)
	defer detach()
	resp, err := t.do(streamCtx, http.MethodGet, opts.URL, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	if !success(resp) {
		raw, _ := readCapped(resp.Body, 64<<10, opts.URL)
		resp.Body.Close()
		cancel()
		client.rejected.record(resp, raw)
		return nil, &HTTPStatusError{Status: resp.StatusCode, Method: http.MethodGet, URL: opts.URL, Body: client.Rejection().Body}
	}
	if kind := ContentType(resp); kind != "text/event-stream" {
		drain(resp.Body)
		cancel()
		return nil, fmt.Errorf("SSE endpoint answered with Content-Type %q, not text/event-stream", kind)
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

// One request with credentials, sent again once after a refusal the authenticator handled
func (t *SSE) do(ctx context.Context, method, target string, body []byte) (*http.Response, error) {
	auth := t.client.opts.Auth
	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, reader)
		if err != nil {
			return nil, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		} else {
			req.Header.Set("Accept", "text/event-stream")
			req.Header.Set("Cache-Control", "no-store")
		}
		for k, v := range t.headers {
			req.Header.Set(k, v)
		}
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
		peek, _ := readCapped(resp.Body, 64<<10, target)
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

// Reads events: the endpoint first, then messages, closing the stream once it is silent too long
func (t *SSE) pump(body io.ReadCloser) {
	defer func() {
		select {
		case <-t.done:
		default:
			t.setErr(errors.New("Connection closed"))
		}
		close(t.recv)
	}()
	reader := NewSSEReader(body, MaxBodyBytes, t.stream)
	watchdog := time.AfterFunc(sseIdle, func() {
		t.setErr(errors.New("SSE stream idle past the read timeout"))
		body.Close()
	})
	defer watchdog.Stop()
	announced := false
	announce := func(err error) {
		if !announced {
			announced = true
			t.readyErr = err
			close(t.ready)
		}
	}
	for {
		event, err := reader.Next()
		if err != nil {
			if !announced {
				announce(fmt.Errorf("SSE stream ended before the endpoint event: %w", err))
			} else if err != io.EOF {
				t.setErr(err)
			}
			return
		}
		watchdog.Reset(sseIdle)
		switch event.Event {
		case "endpoint":
			resolved, err := sameOrigin(t.stream, event.Data)
			if err != nil {
				announce(err)
				return
			}
			t.mu.Lock()
			t.endpoint = resolved
			t.mu.Unlock()
			announce(nil)
		case "message", "":
			if strings.TrimSpace(event.Data) == "" {
				continue
			}
			msgs, err := DecodeMessages([]byte(event.Data))
			if err != nil {
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

// Endpoint resolved against the stream URL, refused on another origin
func sameOrigin(base, endpoint string) (string, error) {
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

func (t *SSE) Send(ctx context.Context, msg *Message) error {
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
	resp, err := t.do(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return err
	}
	raw, _ := readCapped(resp.Body, 64<<10, endpoint)
	resp.Body.Close()
	if !success(resp) {
		t.client.rejected.record(resp, raw)
		return &HTTPStatusError{Status: resp.StatusCode, Method: http.MethodPost, URL: endpoint, Body: t.client.Rejection().Body}
	}
	return nil
}

func (t *SSE) setErr(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err == nil {
		t.err = err
	}
}

func (t *SSE) Receive() <-chan *Message { return t.recv }

func (t *SSE) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

func (t *SSE) Label() string { return "SSE" }

func (t *SSE) Rejection() HTTPRejection { return t.client.Rejection() }

func (t *SSE) Close() error {
	t.once.Do(func() {
		close(t.done)
		if t.body != nil {
			t.body.Close()
		}
		if t.cancel != nil {
			t.cancel()
		}
		t.client.CloseIdleConnections()
	})
	return nil
}
