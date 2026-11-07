package telegram

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyipaddress"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const TelegramAPIHost = "api.telegram.org"

const (
	tcpKeepaliveIdle     = 30 * time.Second
	tcpKeepaliveInterval = 10 * time.Second
	tcpKeepaliveCount    = 3
)

var dohTimeout = 4 * time.Second

type dohProvider struct {
	URL     string
	Params  pyhttpx.Params
	Headers map[string]string
}

var dohProviders = []dohProvider{
	{URL: "https://dns.google/resolve", Params: pyhttpx.Params{{"name", TelegramAPIHost}, {"type", "A"}}, Headers: map[string]string{}},
	{URL: "https://cloudflare-dns.com/dns-query", Params: pyhttpx.Params{{"name", TelegramAPIHost}, {"type", "A"}}, Headers: map[string]string{"Accept": "application/dns-json"}},
}

// Last resort IPv4 Bot API endpoints, also the first connect targets ahead of the dual stack hostname
var SeedFallbackIPs = []string{"149.154.166.110", "149.154.167.220"}

// Non empty secret safe exception repr for diagnostics
func describeTransportError(err error) string {
	return tools.RedactSensitiveTextWith(nil, pyrepr.ExceptionRepr(err), tools.RedactOptions{Force: true})
}

// SO_KEEPALIVE with the idle, interval and count knobs so a half open long poll errors out
func TCPKeepaliveSocketOptions() net.KeepAliveConfig {
	return net.KeepAliveConfig{Enable: true, Idle: tcpKeepaliveIdle, Interval: tcpKeepaliveInterval, Count: tcpKeepaliveCount}
}

// Connection pool bounds of an inner transport, the httpx Limits with zero keepalive connections disabling reuse
type PoolLimits struct {
	MaxConnections          int
	MaxKeepaliveConnections int
	KeepaliveExpiry         time.Duration
}

// httpx Limits defaults of a transport built without explicit limits
var DefaultPoolLimits = PoolLimits{MaxConnections: 100, MaxKeepaliveConnections: 20, KeepaliveExpiry: 5 * time.Second}

// Bound every fallback pool so the wedged endpoint plus seed IPs cannot outgrow the fd limit
var FallbackPoolLimits = PoolLimits{MaxConnections: 8, MaxKeepaliveConnections: 4, KeepaliveExpiry: 5 * time.Second}

// Keyword arguments of an inner transport, nil fields being the ones the caller omitted
type TransportOptions struct {
	Proxy         *string
	Limits        *PoolLimits
	SocketOptions *net.KeepAliveConfig
	TLSConfig     *tls.Config
	Factory       TransportFactory
}

// One connection pool the fallback transport routes a request through, the httpx AsyncHTTPTransport
type InnerTransport interface {
	RoundTrip(req *http.Request) (*http.Response, error)
	Close() error
}

// Builds an inner transport from the shared options
type TransportFactory func(opts TransportOptions) (InnerTransport, error)

// Connect phase failure of a transport, the httpx ConnectTimeout when Timeout is set and ConnectError otherwise
type ConnectError struct {
	Err     error
	Timeout bool
}

func (e *ConnectError) Error() string { return e.Err.Error() }
func (e *ConnectError) Unwrap() error { return e.Err }
func (e *ConnectError) PyTypeName() string {
	if e.Timeout {
		return "ConnectTimeout"
	}
	return "ConnectError"
}

type sniHostnameKey struct{}

// Context carrying the TLS server name a rewritten request keeps, the httpx sni_hostname extension
func withSNIHostname(ctx context.Context, host string) context.Context {
	return context.WithValue(ctx, sniHostnameKey{}, host)
}

// TLS server name a rewritten request asks for, empty when the URL host is the server name
func SNIHostname(ctx context.Context) string {
	host, _ := ctx.Value(sniHostnameKey{}).(string)
	return host
}

type httpTransport struct {
	opts  TransportOptions
	proxy *url.URL
	mu    sync.Mutex
	bySNI map[string]*http.Transport
}

// HTTP/1.1 pool honouring the proxy, limits, keepalive and TLS options and each request's SNI hostname
func NewHTTPTransport(opts TransportOptions) (InnerTransport, error) {
	h := &httpTransport{opts: opts, bySNI: map[string]*http.Transport{}}
	if opts.Proxy != nil && *opts.Proxy != "" {
		u, err := url.Parse(*opts.Proxy)
		if err != nil {
			return nil, err
		}
		h.proxy = u
	}
	return h, nil
}

func (h *httpTransport) transportFor(sni string) *http.Transport {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t, ok := h.bySNI[sni]; ok {
		return t
	}
	limits := DefaultPoolLimits
	if h.opts.Limits != nil {
		limits = *h.opts.Limits
	}
	dialer := &net.Dialer{KeepAlive: -1}
	if h.opts.SocketOptions != nil {
		dialer.KeepAliveConfig = *h.opts.SocketOptions
	}
	tlsConfig := &tls.Config{}
	if h.opts.TLSConfig != nil {
		tlsConfig = h.opts.TLSConfig.Clone()
	}
	if sni != "" {
		tlsConfig.ServerName = sni
	}
	t := &http.Transport{
		DialContext:     dialer.DialContext,
		TLSClientConfig: tlsConfig,
		MaxConnsPerHost: limits.MaxConnections,
		IdleConnTimeout: limits.KeepaliveExpiry,
	}
	if limits.MaxKeepaliveConnections == 0 {
		t.DisableKeepAlives = true
	} else {
		t.MaxIdleConns, t.MaxIdleConnsPerHost = limits.MaxKeepaliveConnections, limits.MaxKeepaliveConnections
	}
	if h.proxy != nil {
		t.Proxy = http.ProxyURL(h.proxy)
	}
	h.bySNI[sni] = t
	return t
}

func (h *httpTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return h.transportFor(SNIHostname(req.Context())).RoundTrip(req)
}

func (h *httpTransport) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, t := range h.bySNI {
		t.CloseIdleConnections()
	}
	clear(h.bySNI)
	return nil
}

// Proxy for the Bot API hosts from TELEGRAM_PROXY, the trusted env or the system proxy
func resolveProxyURL(ctx context.Context, targetHosts []string) (string, error) {
	return gateway.ResolveProxyURL(ctx, "TELEGRAM_PROXY", targetHosts, "")
}

// Bot API transport trying known IPv4 literals first and the dual stack hostname last, Host and SNI kept
type TelegramFallbackTransport struct {
	fallbackIPs   []string
	opts          TransportOptions
	primaryMu     sync.Mutex
	primary       InnerTransport
	primaryClosed bool
	fallbackMu    sync.Mutex
	fallbacks     map[string]InnerTransport
	stickyMu      sync.Mutex
	stickySet     bool
	stickyIP      string
	lastFailure   *[2]string
}

// Fallback transport over the validated IPs with the proxy, pool limits and keepalive defaulted into the options
func NewTelegramFallbackTransport(ctx context.Context, fallbackIPs []string, opts TransportOptions) (*TelegramFallbackTransport, error) {
	t := &TelegramFallbackTransport{fallbackIPs: dedupeStrings(normalizeFallbackIPs(fallbackIPs)), fallbacks: map[string]InnerTransport{}}
	proxyURL, err := resolveProxyURL(ctx, append([]string{TelegramAPIHost}, t.fallbackIPs...))
	if err != nil {
		return nil, err
	}
	if proxyURL != "" && opts.Proxy == nil {
		opts.Proxy = &proxyURL
	}
	if opts.Limits == nil {
		limits := FallbackPoolLimits
		opts.Limits = &limits
	}
	if opts.SocketOptions == nil {
		keep := TCPKeepaliveSocketOptions()
		opts.SocketOptions = &keep
	}
	if opts.Factory == nil {
		opts.Factory = NewHTTPTransport
	}
	t.opts = opts
	if t.primary, err = opts.Factory(opts); err != nil {
		return nil, err
	}
	return t, nil
}

// Options every inner transport is built from, the transport kwargs of hermes
func (t *TelegramFallbackTransport) TransportOptions() TransportOptions { return t.opts }

// Validated and deduplicated IPv4 fallback addresses in order
func (t *TelegramFallbackTransport) FallbackIPs() []string { return slices.Clone(t.fallbackIPs) }

func (t *TelegramFallbackTransport) currentPrimary() InnerTransport {
	t.primaryMu.Lock()
	defer t.primaryMu.Unlock()
	return t.primary
}

func (t *TelegramFallbackTransport) getFallback(ip string) (InnerTransport, error) {
	t.fallbackMu.Lock()
	defer t.fallbackMu.Unlock()
	if transport, ok := t.fallbacks[ip]; ok {
		return transport, nil
	}
	transport, err := t.opts.Factory(t.opts)
	if err != nil {
		return nil, err
	}
	t.fallbacks[ip] = transport
	return transport, nil
}

// Replaces the primary generation after a retryable failure, then closes the failed pool
func (t *TelegramFallbackTransport) resetPrimary(transport InnerTransport) error {
	t.primaryMu.Lock()
	if t.primaryClosed || transport != t.primary {
		t.primaryMu.Unlock()
		return nil
	}
	next, err := t.opts.Factory(t.opts)
	if err != nil {
		t.primaryMu.Unlock()
		return err
	}
	t.primary = next
	t.primaryMu.Unlock()
	if err := transport.Close(); err != nil {
		slog.Debug(fmt.Sprintf("[Telegram] Error closing primary transport: %s", pyrepr.ExceptionMessage(err)))
	}
	return nil
}

// Discards and closes a failed fallback pool so its CLOSE_WAIT socket cannot leak one fd per retry
func (t *TelegramFallbackTransport) resetFallback(ip string) {
	t.fallbackMu.Lock()
	transport, ok := t.fallbacks[ip]
	delete(t.fallbacks, ip)
	t.fallbackMu.Unlock()
	if !ok {
		return
	}
	if err := transport.Close(); err != nil {
		slog.Debug(fmt.Sprintf("[Telegram] Error closing fallback transport %s: %s", ip, pyrepr.ExceptionMessage(err)))
	}
}

// Sticky path first, then the IPv4 literals, the hostname as the empty entry last
func (t *TelegramFallbackTransport) attemptOrder() []string {
	t.stickyMu.Lock()
	defer t.stickyMu.Unlock()
	var order []string
	if t.stickySet {
		order = append(order, t.stickyIP)
	}
	for _, ip := range t.fallbackIPs {
		if !slices.Contains(order, ip) {
			order = append(order, ip)
		}
	}
	if !slices.Contains(order, "") {
		order = append(order, "")
	}
	return order
}

func (t *TelegramFallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.ToLower(req.URL.Hostname()) != TelegramAPIHost || len(t.fallbackIPs) == 0 {
		return t.currentPrimary().RoundTrip(req)
	}
	getBody := req.GetBody
	if getBody == nil && req.Body != nil && req.Body != http.NoBody {
		data, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		getBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	}
	var lastErr error
	for _, ip := range t.attemptOrder() {
		candidate := req.Clone(req.Context())
		if ip != "" {
			candidate = rewriteRequestForIP(req, ip)
		}
		if getBody != nil {
			body, err := getBody()
			if err != nil {
				return nil, err
			}
			candidate.Body, candidate.GetBody = body, getBody
		}
		transport := t.currentPrimary()
		if ip != "" {
			var err error
			if transport, err = t.getFallback(ip); err != nil {
				return nil, err
			}
		}
		resp, err := transport.RoundTrip(candidate)
		if err == nil {
			t.recordSuccess(ip, lastErr != nil)
			return resp, nil
		}
		lastErr = err
		if !isRetryableConnectError(err) {
			return nil, err
		}
		path := ip
		if path == "" {
			path = TelegramAPIHost
		}
		failure := describeTransportError(err)
		t.recordFailure(ip, path, failure)
		if ip == "" {
			if rerr := t.resetPrimary(transport); rerr != nil {
				return nil, rerr
			}
			slog.Warn(fmt.Sprintf("[Telegram] Dual-stack api.telegram.org path failed (%s)", failure))
			continue
		}
		slog.Warn(fmt.Sprintf("[Telegram] IPv4 Telegram API IP %s failed: %s", ip, failure))
		t.resetFallback(ip)
	}
	if lastErr == nil {
		return nil, errors.New("RuntimeError: All Telegram fallback IPs exhausted but no error was recorded")
	}
	return nil, lastErr
}

// Logs a recovery after a failed path and makes the working path sticky
func (t *TelegramFallbackTransport) recordSuccess(ip string, afterError bool) {
	t.stickyMu.Lock()
	defer t.stickyMu.Unlock()
	if t.lastFailure != nil {
		failed := *t.lastFailure
		t.lastFailure = nil
		via := ip
		if via == "" {
			via = TelegramAPIHost
		}
		slog.Info(fmt.Sprintf("[Telegram] Telegram API transport recovered via %s after %s failed: %s", via, failed[0], failed[1]))
	}
	if t.stickySet && t.stickyIP == ip {
		return
	}
	t.stickySet, t.stickyIP = true, ip
	if ip == "" {
		return
	}
	msg := fmt.Sprintf("[Telegram] Using sticky IPv4 Telegram API path %s (dual-stack hostname tried last — #87015)", ip)
	if afterError {
		slog.Warn(msg)
	} else {
		slog.Info(msg)
	}
}

// Remembers the failed path and drops a sticky path that just failed
func (t *TelegramFallbackTransport) recordFailure(ip, path, failure string) {
	t.stickyMu.Lock()
	defer t.stickyMu.Unlock()
	t.lastFailure = &[2]string{path, failure}
	if t.stickySet && t.stickyIP == ip {
		t.stickySet, t.stickyIP = false, ""
		shown := ip
		if shown == "" {
			shown = "api.telegram.org"
		}
		slog.Warn(fmt.Sprintf("[Telegram] Sticky Telegram path %s failed; re-walking IPv4 literals before the hostname", shown))
	}
}

// Closes the primary and every fallback pool, the primary marked closed so no reset rebuilds it
func (t *TelegramFallbackTransport) Close() error {
	t.primaryMu.Lock()
	t.primaryClosed = true
	primary := t.primary
	t.primaryMu.Unlock()
	if err := primary.Close(); err != nil {
		return err
	}
	t.fallbackMu.Lock()
	transports := make([]InnerTransport, 0, len(t.fallbacks))
	for _, transport := range t.fallbacks {
		transports = append(transports, transport)
	}
	clear(t.fallbacks)
	t.fallbackMu.Unlock()
	for _, transport := range transports {
		if err := transport.Close(); err != nil {
			return err
		}
	}
	return nil
}

// Public IPv4 addresses of the values, invalid, IPv6 and internal ones skipped with a warning
func normalizeFallbackIPs(values []string) []string {
	var normalized []string
	for _, value := range values {
		raw := pystr.Strip(value)
		if raw == "" {
			continue
		}
		addr, ok := pyipaddress.IPAddress(raw)
		switch {
		case !ok:
			slog.Warn(fmt.Sprintf("Ignoring invalid Telegram fallback IP: %s", pyrepr.Repr(raw)))
		case !addr.Is4():
			slog.Warn(fmt.Sprintf("Ignoring non-IPv4 Telegram fallback IP: %s", raw))
		case pyipaddress.IsPrivate(addr) || pyipaddress.IsLoopback(addr) || pyipaddress.IsLinkLocal(addr) || addr.IsUnspecified():
			slog.Warn(fmt.Sprintf("Ignoring private/internal Telegram fallback IP: %s", raw))
		default:
			normalized = append(normalized, addr.String())
		}
	}
	return normalized
}

// Fallback IPs of a comma separated env value, empty for an empty value
func ParseFallbackIPEnv(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	for i, part := range parts {
		parts[i] = pystr.Strip(part)
	}
	return normalizeFallbackIPs(parts)
}

func dedupeStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}

// IPv4 addresses the OS resolver gives for the Bot API host, empty on any failure
func resolveSystemDNS() []string {
	addrs, err := net.DefaultResolver.LookupIP(context.Background(), "ip4", TelegramAPIHost)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.String())
	}
	return dedupeStrings(out)
}

// A record addresses one DoH provider returns, empty when the query or its answer fails
func queryDoHProvider(client *http.Client, p dohProvider) []string {
	ips, err := queryDoH(client, p)
	if err != nil {
		slog.Debug(fmt.Sprintf("DoH query to %s failed: %s", p.URL, pyrepr.ExceptionMessage(err)))
		return nil
	}
	return ips
}

func queryDoH(client *http.Client, p dohProvider) ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, pyhttpx.MergeParams(p.URL, p.Params), nil)
	if err != nil {
		return nil, err
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	resp, err := pyhttpx.DoTimeout(client, req, pyhttpx.Timeout{Connect: dohTimeout, Read: dohTimeout, Write: dohTimeout, Pool: dohTimeout})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, errors.New(pyhttpx.RaiseForStatusMessage(resp.StatusCode, resp.URL, resp.Header.Get("Location")))
	}
	data, err := resp.JSON()
	if err != nil {
		return nil, err
	}
	obj, ok := data.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyTypeName(data))
	}
	answers, ok := obj.Get("Answer")
	if !ok {
		return []string{}, nil
	}
	list, ok := answers.([]any)
	if !ok {
		return nil, fmt.Errorf("TypeError: '%s' object is not iterable", pyTypeName(answers))
	}
	ips := []string{}
	for _, item := range list {
		answer, ok := item.(*pyjson.Object)
		if !ok {
			return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyTypeName(item))
		}
		if typ, _ := answer.Get("type"); !pyEqualsOne(typ) {
			continue
		}
		rawValue, present := answer.Get("data")
		if !present {
			rawValue = ""
		}
		text, ok := rawValue.(string)
		if !ok {
			return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'strip'", pyTypeName(rawValue))
		}
		raw := pystr.Strip(text)
		if _, ok := pyipaddress.IPAddress(raw); !ok {
			continue
		}
		ips = append(ips, raw)
	}
	return ips, nil
}

// Whether a decoded JSON value compares equal to the int 1 like Python, true and 1.0 included
func pyEqualsOne(v any) bool {
	switch n := v.(type) {
	case bool:
		return n
	case int64:
		return n == 1
	case float64:
		return n == 1
	}
	return false
}

func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case float64:
		return "float"
	case []any:
		return "list"
	case *pyjson.Object:
		return "dict"
	}
	return "int"
}

// Bot API addresses from Google and Cloudflare DoH in order, the seed IPs when DoH yields nothing usable
func DiscoverFallbackIPs(ctx context.Context) []string {
	client := newDoHClient()
	defer client.CloseIdleConnections()
	return discoverFallbackIPs(ctx, client, resolveSystemDNS)
}

// DoH client verifying with the shared platform trust context and honouring the proxy env
func newDoHClient() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: provider.PlatformTLSConfig()}, CheckRedirect: pyhttpx.RedirectPolicy(false)}
}

func discoverFallbackIPs(ctx context.Context, client *http.Client, resolveSystem func() []string) []string {
	systemDone := make(chan []string, 1)
	go func() { systemDone <- resolveSystem() }()
	results := make([][]string, len(dohProviders))
	var wg sync.WaitGroup
	for i, p := range dohProviders {
		wg.Go(func() { results[i] = queryDoHProvider(client, p) })
	}
	wg.Wait()
	var systemIPs []string
	timer := time.NewTimer(dohTimeout)
	defer timer.Stop()
	select {
	case systemIPs = <-systemDone:
	case <-timer.C:
		slog.Debug(fmt.Sprintf("System-DNS resolution for %s did not complete in time", TelegramAPIHost))
	case <-ctx.Done():
		slog.Debug(fmt.Sprintf("System-DNS resolution for %s did not complete in time", TelegramAPIHost))
	}
	var dohIPs []string
	for _, r := range results {
		dohIPs = append(dohIPs, r...)
	}
	if validated := normalizeFallbackIPs(dedupeStrings(dohIPs)); len(validated) > 0 {
		slog.Debug("Discovered Telegram fallback IPs via DoH: " + strings.Join(validated, ", "))
		return validated
	}
	system := strings.Join(systemIPs, ", ")
	if system == "" {
		system = "unknown"
	}
	slog.Info(fmt.Sprintf("DoH discovery yielded no usable IPs (system DNS: %s); using seed fallback IPs %s", system, strings.Join(SeedFallbackIPs, ", ")))
	return slices.Clone(SeedFallbackIPs)
}

// Request aimed at the IP literal with the original host kept as the Host header and SNI hostname
func rewriteRequestForIP(req *http.Request, ip string) *http.Request {
	original := req.URL.Hostname()
	if original == "" {
		original = TelegramAPIHost
	}
	out := req.Clone(withSNIHostname(req.Context(), original))
	host := ip
	if port := req.URL.Port(); port != "" {
		host = net.JoinHostPort(ip, port)
	}
	out.URL.Host = host
	out.Host = original
	return out
}

// Whether the failure happened while connecting, the httpx ConnectError and ConnectTimeout classes
func isRetryableConnectError(err error) bool {
	var ce *ConnectError
	if errors.As(err, &ce) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && (op.Op == "dial" || op.Op == "proxyconnect") {
		return true
	}
	var recordErr tls.RecordHeaderError
	var alertErr tls.AlertError
	var verifyErr *tls.CertificateVerificationError
	var authErr x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalidErr x509.CertificateInvalidError
	if errors.As(err, &recordErr) || errors.As(err, &alertErr) || errors.As(err, &verifyErr) || errors.As(err, &authErr) || errors.As(err, &hostErr) || errors.As(err, &invalidErr) {
		return true
	}
	return strings.Contains(err.Error(), "TLS handshake timeout")
}
