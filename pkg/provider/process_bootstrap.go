package provider

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Stagger between dual stack connection attempts
const happyEyeballsDelay = 250 * time.Millisecond

// Connect, write and pool timeouts of every keepalive client
const (
	keepaliveConnectTimeout = 15 * time.Second
	keepaliveWriteTimeout   = 15 * time.Second
	keepalivePoolTimeout    = 10 * time.Second
)

// Error a closed keepalive client or transport view raises on any request
const keepaliveClosedMessage = "Cannot send a request, as the client has been closed."

// Resolver and single address connect seams of the dual stack dialer
var (
	happyEyeballsLookupFn = func(ctx context.Context, host string) ([]net.IP, error) {
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		ips := make([]net.IP, 0, len(addrs))
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
		return ips, nil
	}
	happyEyeballsConnectFn = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{FallbackDelay: -1}).DialContext(ctx, network, address)
	}
)

// Timeout raised by a connect, pool wait or write that ran out of time
type keepaliveTimeoutError struct{ msg string }

func (e *keepaliveTimeoutError) Error() string   { return e.msg }
func (e *keepaliveTimeoutError) Timeout() bool   { return true }
func (e *keepaliveTimeoutError) Temporary() bool { return true }

// Round robins the resolved address families with duplicates dropped and resolver order kept inside each family
func interleaveDialAddrs(ips []net.IP) []net.IP {
	var order []bool
	queues := map[bool][]net.IP{}
	seen := map[string]bool{}
	for _, ip := range ips {
		if seen[ip.String()] {
			continue
		}
		seen[ip.String()] = true
		v4 := ip.To4() != nil
		if _, ok := queues[v4]; !ok {
			order = append(order, v4)
		}
		queues[v4] = append(queues[v4], ip)
	}
	var out []net.IP
	for len(out) < len(seen) {
		for _, family := range order {
			if q := queues[family]; len(q) > 0 {
				out = append(out, q[0])
				queues[family] = q[1:]
			}
		}
	}
	return out
}

type dialAttempt struct {
	conn net.Conn
	err  error
}

// Staggered dual stack connect that launches the next address every 250ms or at once after a failure
func happyEyeballsDial(ctx context.Context, network, address string, timeout time.Duration) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	ips, err := happyEyeballsLookupFn(ctx, host)
	if err != nil {
		return nil, err
	}
	pending := interleaveDialAddrs(ips)
	if len(pending) == 0 {
		return nil, fmt.Errorf("getaddrinfo returned no addresses for %s", host)
	}
	attemptCtx, cancel := context.WithCancel(ctx)
	results := make(chan dialAttempt, len(pending))
	active := 0
	defer func() {
		cancel()
		go func(n int) {
			for ; n > 0; n-- {
				if r := <-results; r.conn != nil {
					_ = r.conn.Close()
				}
			}
		}(active)
	}()
	var lastErr error
	nextLaunch := time.Now()
	for len(pending) > 0 || active > 0 {
		now := time.Now()
		if !deadline.IsZero() && !now.Before(deadline) {
			return nil, &keepaliveTimeoutError{msg: "timed out"}
		}
		if len(pending) > 0 && !now.Before(nextLaunch) {
			target := net.JoinHostPort(pending[0].String(), port)
			pending = pending[1:]
			active++
			go func() {
				conn, err := happyEyeballsConnectFn(attemptCtx, network, target)
				results <- dialAttempt{conn: conn, err: err}
			}()
			nextLaunch = now.Add(happyEyeballsDelay)
			continue
		}
		wait := time.Duration(-1)
		if !deadline.IsZero() {
			wait = deadline.Sub(now)
		}
		if len(pending) > 0 {
			if until := nextLaunch.Sub(now); wait < 0 || until < wait {
				wait = until
			}
		}
		var timer <-chan time.Time
		var t *time.Timer
		if wait >= 0 {
			t = time.NewTimer(wait)
			timer = t.C
		}
		select {
		case r := <-results:
			active--
			if r.err == nil {
				return r.conn, nil
			}
			lastErr = r.err
			if active == 0 && len(pending) > 0 {
				nextLaunch = time.Now()
			}
		case <-timer:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if t != nil {
			t.Stop()
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("Could not connect to %s:%s", host, port)
}

// First proxy URL of HTTPS_PROXY, HTTP_PROXY or ALL_PROXY in either case, empty when none is set
func getProxyFromEnv() string {
	if value := FirstProxyEnvValue(); value != "" {
		return normalizeProxyURL(value)
	}
	return ""
}

// Environment proxy unless NO_PROXY excludes this base URL by CIDR, wildcard or host and port
func getProxyForBaseURL(baseURL string) string {
	proxy := getProxyFromEnv()
	if proxy == "" || baseURL == "" {
		return proxy
	}
	raw := strings.TrimSpace(baseURL)
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	if ShouldBypassProxy([]string{raw}, nil) {
		return ""
	}
	return proxy
}

// True for the official ChatGPT Codex backend whose sync transport races IPv6 and IPv4
func usesCodexCloudTransport(baseURL string) bool {
	return baseURLHostname(baseURL) == "chatgpt.com" && strings.Contains(strings.ToLower(baseURL), "/backend-api/codex")
}

// Connection pool sizing of one keepalive transport
type keepaliveLimits struct {
	maxKeepalive int
	maxConns     int
	expiry       time.Duration
}

// TCP connector of one keepalive transport bounding its total connections and each write
type keepaliveDialer struct {
	racing  atomic.Bool
	proxied bool
	slots   chan struct{}
	live    auxConnSet
}

func (d *keepaliveDialer) acquire(ctx context.Context) error {
	timer := time.NewTimer(keepalivePoolTimeout)
	defer timer.Stop()
	select {
	case d.slots <- struct{}{}:
		return nil
	case <-timer.C:
		return &keepaliveTimeoutError{msg: "timed out waiting for a connection from the pool"}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Opens one pooled TCP connection, raced across address families when the transport asks for it
func (d *keepaliveDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if err := d.acquire(ctx); err != nil {
		return nil, err
	}
	var conn net.Conn
	var err error
	if d.racing.Load() {
		conn, err = happyEyeballsDial(ctx, network, address, keepaliveConnectTimeout)
	} else {
		conn, err = (&net.Dialer{Timeout: keepaliveConnectTimeout, FallbackDelay: -1}).DialContext(ctx, network, address)
	}
	if err != nil {
		<-d.slots
		return nil, err
	}
	kc := &keepaliveConn{Conn: conn}
	kc.release = func() {
		d.live.remove(kc)
		<-d.slots
	}
	d.live.add(kc)
	return kc, nil
}

// TCP connect plus TLS handshake under one connect deadline with HTTP/1.1 negotiated
func (d *keepaliveDialer) dialTLS(cfg *tls.Config) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		connectCtx, cancel := context.WithTimeout(ctx, keepaliveConnectTimeout)
		defer cancel()
		raw, err := d.DialContext(connectCtx, network, address)
		if err != nil {
			return nil, err
		}
		conf := cfg.Clone()
		if conf.ServerName == "" {
			host, _, _ := net.SplitHostPort(address)
			conf.ServerName = host
		}
		conf.NextProtos = []string{"http/1.1"}
		conn := tls.Client(raw, conf)
		if err := conn.HandshakeContext(connectCtx); err != nil {
			_ = raw.Close()
			return nil, err
		}
		return conn, nil
	}
}

// Pooled connection that bounds every write and frees its pool slot exactly once on close
type keepaliveConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *keepaliveConn) Write(p []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(keepaliveWriteTimeout))
	n, err := c.Conn.Write(p)
	_ = c.Conn.SetWriteDeadline(time.Time{})
	return n, err
}

// Raw connection under the pool wrapper for socket level shutdown
func (c *keepaliveConn) NetConn() net.Conn { return c.Conn }

func (c *keepaliveConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// One HTTP/1.1 transport with its pool limits, TLS context, optional fixed proxy and connector
type keepaliveTransport struct {
	transport *http.Transport
	dialer    *keepaliveDialer
}

func newKeepaliveTransport(tlsConfig *tls.Config, limits keepaliveLimits, proxy *url.URL, racing bool) *keepaliveTransport {
	d := &keepaliveDialer{proxied: proxy != nil, slots: make(chan struct{}, limits.maxConns)}
	d.racing.Store(racing)
	t := &http.Transport{
		DialContext:           d.DialContext,
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   keepaliveConnectTimeout,
		MaxIdleConns:          limits.maxKeepalive,
		MaxIdleConnsPerHost:   limits.maxKeepalive,
		IdleConnTimeout:       limits.expiry,
		ExpectContinueTimeout: time.Second,
	}
	if proxy != nil {
		t.Proxy = http.ProxyURL(proxy)
	} else {
		t.DialTLSContext = d.dialTLS(tlsConfig)
	}
	auxPoolConnSets.Store(t, &d.live)
	return &keepaliveTransport{transport: t, dialer: d}
}

// Identity under which sync direct transports are pooled process wide
type sharedTransportKey struct {
	scheme   string
	insecure bool
	config   *tls.Config
	proxy    string
	codex    bool
}

// Process wide pool of sync direct transports, bounded so callers past the cap get a private transport
var (
	sharedTransportsMu  sync.Mutex
	sharedTransports    = map[sharedTransportKey]*keepaliveTransport{}
	sharedTransportsMax = 32
)

func sharedTransportKeyFor(scheme, baseURL string, verify TLSVerify, proxy string) sharedTransportKey {
	key := sharedTransportKey{scheme: scheme, insecure: verify.Insecure, proxy: proxy, codex: usesCodexCloudTransport(baseURL)}
	if !verify.Insecure {
		key.config = verify.Config
	}
	return key
}

func getSharedTransport(key sharedTransportKey, build func() *keepaliveTransport) *keepaliveTransport {
	sharedTransportsMu.Lock()
	defer sharedTransportsMu.Unlock()
	if t, ok := sharedTransports[key]; ok {
		return t
	}
	t := build()
	if len(sharedTransports) < sharedTransportsMax {
		sharedTransports[key] = t
	}
	return t
}

// Really closes every process shared transport and reports how many there were
func closeSharedTransports() int {
	sharedTransportsMu.Lock()
	transports := make([]*keepaliveTransport, 0, len(sharedTransports))
	for _, t := range sharedTransports {
		transports = append(transports, t)
	}
	sharedTransports = map[sharedTransportKey]*keepaliveTransport{}
	sharedTransportsMu.Unlock()
	for _, t := range transports {
		t.transport.CloseIdleConnections()
	}
	return len(transports)
}

// Per client view of a shared transport that absorbs close and tracks its own in flight connections
type sharedTransportView struct {
	inner    *keepaliveTransport
	closed   atomic.Bool
	mu       sync.Mutex
	seq      uint64
	inflight map[uint64]net.Conn
}

func newSharedTransportView(inner *keepaliveTransport) *sharedTransportView {
	return &sharedTransportView{inner: inner, inflight: map[uint64]net.Conn{}}
}

func (v *sharedTransportView) RoundTrip(req *http.Request) (*http.Response, error) {
	if v.closed.Load() {
		return nil, &AuxRuntimeError{Message: keepaliveClosedMessage}
	}
	v.mu.Lock()
	v.seq++
	id := v.seq
	v.mu.Unlock()
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		v.mu.Lock()
		v.inflight[id] = info.Conn
		v.mu.Unlock()
	}}
	resp, err := v.inner.transport.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	if err != nil {
		v.release(id)
		return nil, err
	}
	resp.Body = &ownedBody{ReadCloser: resp.Body, release: func() { v.release(id) }}
	return resp, nil
}

func (v *sharedTransportView) release(id uint64) {
	v.mu.Lock()
	delete(v.inflight, id)
	v.mu.Unlock()
}

// Connections this view's own requests hold right now, never another client's on the shared pool
func (v *sharedTransportView) inflightConns() []net.Conn {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]net.Conn, 0, len(v.inflight))
	for _, c := range v.inflight {
		out = append(out, c)
	}
	return out
}

// Response body that hands its connection back to the owner bookkeeping at EOF, error or close
type ownedBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *ownedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(b.release)
	}
	return n, err
}

func (b *ownedBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

// Client transport routing each scheme to its mount, zstd compressing Codex request bodies when asked
type keepaliveClientTransport struct {
	mounts       map[string]http.RoundTripper
	private      []*keepaliveTransport
	compressBase string
	closed       atomic.Bool
}

func (k *keepaliveClientTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if k.closed.Load() {
		return nil, &AuxRuntimeError{Message: keepaliveClosedMessage}
	}
	rt := k.mounts[req.URL.Scheme]
	if rt == nil {
		return nil, fmt.Errorf("Request URL has an unsupported protocol '%s://'.", req.URL.Scheme)
	}
	if k.compressBase != "" && req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/responses") &&
		req.Header.Get("Content-Encoding") == "" && req.Body != nil && req.Body != http.NoBody {
		body, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		clone := req.Clone(req.Context())
		clone.Body = io.NopCloser(bytes.NewReader(body))
		clone.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		codexCompressRequest(clone, k.compressBase, body)
		req = clone
	}
	return rt.RoundTrip(req)
}

// Marks the client closed, shared views stop serving while the shared pools keep running
func (k *keepaliveClientTransport) Close() error {
	k.closed.Store(true)
	for _, rt := range k.mounts {
		if v, ok := rt.(*sharedTransportView); ok {
			v.closed.Store(true)
		}
	}
	for _, t := range k.private {
		t.transport.CloseIdleConnections()
	}
	return nil
}

// Drops idle connections of the private transports, shared pools reap their own idle connections
func (k *keepaliveClientTransport) CloseIdleConnections() {
	for _, t := range k.private {
		t.transport.CloseIdleConnections()
	}
}

// Closes an HTTP client built here or drops the idle connections of any other client
func closeHTTPClient(c *http.Client) {
	if c == nil {
		return
	}
	if closer, ok := c.Transport.(interface{ Close() error }); ok {
		_ = closer.Close()
		return
	}
	c.CloseIdleConnections()
}

func supportedProxyScheme(scheme string) bool {
	switch strings.ToLower(scheme) {
	case "http", "https", "socks5", "socks5h":
		return true
	}
	return false
}

// Keepalive HTTP client with the environment only proxy policy, nil when it cannot be built
func buildKeepaliveHTTPClient(baseURL string, asyncMode bool, verify TLSVerify) *http.Client {
	proxy := getProxyForBaseURL(baseURL)
	var proxyURL *url.URL
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil || !supportedProxyScheme(u.Scheme) || u.Host == "" {
			return nil
		}
		proxyURL = u
	}
	tlsConfig := verify.Config
	if tlsConfig == nil {
		if verify.Insecure {
			tlsConfig = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
		} else {
			tlsConfig = PlatformTLSConfig()
		}
	}
	limits := keepaliveLimits{maxKeepalive: 20, maxConns: 100, expiry: 20 * time.Second}
	ct := &keepaliveClientTransport{mounts: map[string]http.RoundTripper{}}
	if isOfficialCodexBaseURL(baseURL) {
		ct.compressBase = baseURL
	}
	switch {
	case proxyURL != nil:
		t := newKeepaliveTransport(tlsConfig, limits, proxyURL, asyncMode)
		ct.private = append(ct.private, t)
		ct.mounts["http"], ct.mounts["https"] = t.transport, t.transport
	case asyncMode:
		for _, scheme := range []string{"http", "https"} {
			t := newKeepaliveTransport(tlsConfig, limits, nil, true)
			ct.private = append(ct.private, t)
			ct.mounts[scheme] = t.transport
		}
	default:
		racing := usesCodexCloudTransport(baseURL)
		direct := keepaliveLimits{maxKeepalive: 50, maxConns: 1000, expiry: 20 * time.Second}
		for _, scheme := range []string{"http", "https"} {
			shared := getSharedTransport(sharedTransportKeyFor(scheme, baseURL, verify, ""), func() *keepaliveTransport {
				return newKeepaliveTransport(tlsConfig, direct, nil, racing)
			})
			ct.mounts[scheme] = newSharedTransportView(shared)
		}
	}
	return &http.Client{Transport: ct, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// BuildKeepaliveHTTPClient is the keepalive client with the environment only proxy policy, nil when it cannot be built
func BuildKeepaliveHTTPClient(baseURL string, asyncMode bool, verify TLSVerify) *http.Client {
	return buildKeepaliveHTTPClient(baseURL, asyncMode, verify)
}

// Races IPv6 and IPv4 on every direct transport of a ready built client, proxy pools keep their serial connect
func enableHappyEyeballsOnClient(client *http.Client) {
	if client == nil {
		return
	}
	switch t := client.Transport.(type) {
	case *keepaliveClientTransport:
		for _, rt := range t.mounts {
			if v, ok := rt.(*sharedTransportView); ok && !v.inner.dialer.proxied {
				v.inner.dialer.racing.Store(true)
			}
		}
		for _, p := range t.private {
			if !p.dialer.proxied {
				p.dialer.racing.Store(true)
			}
		}
	case *http.Transport:
		if t.Proxy == nil {
			t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				return happyEyeballsDial(ctx, network, address, 0)
			}
		}
	}
}

// Stdio writer that swallows broken pipe and closed handle errors so console output never crashes the agent
type safeWriter struct{ inner io.Writer }

func (w safeWriter) Write(p []byte) (int, error) {
	n, err := w.inner.Write(p)
	if err != nil {
		return len(p), nil
	}
	return n, nil
}

// Keeps the process alive when its stdout or stderr pipe goes away, writes then fail quietly
func installSafeStdio() {
	signal.Ignore(syscall.SIGPIPE)
}

// Ignores SIGPIPE at agent startup so a vanished stdout or stderr pipe never kills the process
func InstallSafeStdio() { installSafeStdio() }
