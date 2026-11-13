// Outbound HTTP transport every feature shares, with proxy routing, OS trust, phase deadlines, retries and decoding
package httpclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
)

// Settings of one transport, zero fields meaning OS trust, no deadlines and no redirects followed
type Options struct {
	Proxy          ProxyOptions
	TLS            TLSOptions
	Timeouts       Timeouts
	Pool           Pool
	MaxRedirects   int
	ForwardHeaders bool
	UserAgent      string
	Resolver       Resolver
	PreferIPv4     bool
}

// Round tripper that sends each request directly or through its proxy with every phase bounded
type Transport struct {
	opts   Options
	dial   *dialer
	mu     sync.Mutex
	routes map[routeKey]*http.Transport
	system systemCache
}

// Way a request reaches its target
type routeKind uint8

const (
	routeDirect routeKind = iota
	routeForward
	routeTunnel
)

type routeKey struct {
	kind  routeKind
	proxy string
}

// Builds a transport, loading every trust source and checking the configured proxy URL
func New(o Options) (*Transport, error) {
	if o.UserAgent == "" {
		o.UserAgent = Product
	}
	o.Pool = o.Pool.filled()
	if o.Proxy.URL != "" {
		if _, err := parseProxy(o.Proxy.URL, ""); err != nil {
			return nil, err
		}
	}
	target, proxyTrust, err := buildTrust(o.TLS)
	if err != nil {
		return nil, err
	}
	return &Transport{
		opts: o,
		dial: &dialer{
			resolver:   o.Resolver,
			connect:    o.Timeouts.Connect,
			handshake:  o.Timeouts.TLS,
			write:      o.Timeouts.Write,
			preferIPv4: o.PreferIPv4,
			agent:      o.UserAgent,
			target:     target,
			proxy:      proxyTrust,
		},
		routes: map[routeKey]*http.Transport{},
	}, nil
}

// HTTP client over this transport, redirects followed as its options allow
func (t *Transport) Client() *http.Client {
	return &http.Client{
		Transport:     t,
		CheckRedirect: redirectPolicy(t.opts.MaxRedirects, t.opts.ForwardHeaders),
	}
}

// TLS settings verifying a peer as host with the trust toward targets, for clients that run their own handshake
func (t *Transport) TLSConfig(host string) *tls.Config { return t.dial.target.config(host) }

// Closes idle connections of every route
func (t *Transport) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, rt := range t.routes {
		rt.CloseIdleConnections()
	}
}

// Sends one request over its first reachable route with the body decoded and every phase deadline armed
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil {
		closeBody(req)
		return nil, errors.New("request has no URL")
	}
	if scheme := strings.ToLower(req.URL.Scheme); scheme != "http" && scheme != "https" {
		closeBody(req)
		return nil, fmt.Errorf("%w, got %q", ErrUnsupportedScheme, req.URL.Scheme)
	}
	if req.URL.Hostname() == "" {
		closeBody(req)
		return nil, errors.New("request URL has no host")
	}
	ctx, release := within(req.Context(), t.opts.Timeouts.Exchange)
	routes, err := t.routesFor(ctx, req.URL)
	if err != nil {
		release()
		closeBody(req)
		return nil, err
	}
	out := req.Clone(req.Context())
	if out.Header == nil {
		out.Header = http.Header{}
	}
	if out.Header.Get("User-Agent") == "" {
		out.Header.Set("User-Agent", t.opts.UserAgent)
	}
	if out.Header.Get("Accept-Encoding") == "" && out.Header.Get("Range") == "" && out.Method != http.MethodHead {
		out.Header.Set("Accept-Encoding", acceptEncoding)
	}
	var lastErr error
	for i, proxyURL := range routes {
		if i > 0 && !rewind(out) {
			break
		}
		resp, err := t.attempt(ctx, out, proxyURL, release)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		var pe *ProxyError
		if !errors.As(err, &pe) || pe.Status != 0 {
			break
		}
	}
	release()
	return nil, lastErr
}

// Closes a request body the transport will not send, as the RoundTripper contract requires
func closeBody(req *http.Request) {
	if req.Body != nil {
		req.Body.Close()
	}
}

// Fresh body for another route, false when the body cannot be replayed
func rewind(req *http.Request) bool {
	if req.Body == nil || req.Body == http.NoBody {
		return true
	}
	if req.GetBody == nil {
		return false
	}
	body, err := req.GetBody()
	if err != nil {
		return false
	}
	req.Body = body
	return true
}

// One try over one route with its own watchdog, taking over the exchange deadline when it succeeds
func (t *Transport) attempt(bounded context.Context, req *http.Request, proxyURL *url.URL, release context.CancelFunc) (*http.Response, error) {
	rt, forward := t.transportFor(req.URL, proxyURL)
	ctx, cancel := context.WithCancel(bounded)
	x := &exchange{host: dialTarget(req.URL), limits: t.opts.Timeouts, parent: req.Context(), bounded: bounded, cancel: cancel}
	if forward {
		x.forward = proxyURL
	}
	ctx = context.WithValue(ctx, exchangeKey{}, x)
	ctx = httptrace.WithClientTrace(ctx, x.trace())
	resp, err := rt.RoundTrip(req.WithContext(ctx))
	if err != nil {
		wrapped := x.wrap(err)
		x.finish()
		return nil, wrapped
	}
	x.release = release
	if resp.StatusCode == http.StatusSwitchingProtocols {
		x.finish()
		return resp, nil
	}
	x.arm(PhaseBody, t.opts.Timeouts.Idle)
	resp.Body = &watchedBody{rc: resp.Body, x: x}
	decodeResponse(resp, x)
	return resp, nil
}

// Pooled transport of a route, reporting whether it forwards plain http through a proxy
func (t *Transport) transportFor(target, proxyURL *url.URL) (*http.Transport, bool) {
	key := routeKey{kind: routeDirect}
	if proxyURL != nil {
		key = routeKey{kind: routeTunnel, proxy: proxyURL.String()}
		if strings.HasPrefix(proxyURL.Scheme, "http") && strings.EqualFold(target.Scheme, "http") {
			key.kind = routeForward
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if rt, ok := t.routes[key]; ok {
		return rt, key.kind == routeForward
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	rt := &http.Transport{
		DisableCompression:  true,
		MaxIdleConns:        t.opts.Pool.MaxIdle,
		MaxIdleConnsPerHost: t.opts.Pool.MaxIdle,
		MaxConnsPerHost:     t.opts.Pool.MaxOpen,
		IdleConnTimeout:     t.opts.Pool.IdleTimeout,
		Protocols:           protocols,
	}
	d := t.dial
	switch key.kind {
	case routeDirect:
		rt.DialContext = d.direct
		rt.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := d.direct(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return d.secure(ctx, conn, addr)
		}
	case routeForward:
		rt.Proxy = http.ProxyURL(&url.URL{Scheme: "http", User: proxyURL.User, Host: proxyAddr(proxyURL)})
		rt.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { return d.forward(ctx, proxyURL) }
	case routeTunnel:
		rt.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) { return d.through(ctx, proxyURL, addr) }
		rt.DialTLSContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
			conn, err := d.through(ctx, proxyURL, addr)
			if err != nil {
				return nil, err
			}
			return d.secure(ctx, conn, addr)
		}
	}
	t.routes[key] = rt
	return rt, key.kind == routeForward
}

// TCP connection to a target URL directly or tunnelled through its proxy, for WebSocket and other raw clients
func (t *Transport) DialFor(ctx context.Context, target *url.URL) (net.Conn, error) {
	routes, err := t.routesFor(ctx, target)
	if err != nil {
		return nil, err
	}
	addr := dialTarget(target)
	var lastErr error
	for _, proxyURL := range routes {
		var conn net.Conn
		if proxyURL == nil {
			conn, err = t.dial.direct(ctx, "tcp", addr)
		} else {
			conn, err = t.dial.through(ctx, proxyURL, addr)
		}
		if err == nil {
			return conn, nil
		}
		phase := PhaseConnect
		var pe *ProxyError
		if errors.As(err, &pe) {
			phase = PhaseProxy
		}
		lastErr = &Error{Phase: phase, Host: addr, Err: err}
		if pe == nil || pe.Status != 0 {
			break
		}
	}
	return nil, lastErr
}
