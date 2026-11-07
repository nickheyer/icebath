package telegram

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

type fakeCall struct{ urlHost, hostHeader, sni, path string }

type fakeNetTransport struct {
	mu       *sync.Mutex
	calls    *[]fakeCall
	behavior map[string]any
	closed   bool
	closeLog *[]*fakeNetTransport
}

func (f *fakeNetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	host := req.Host
	if host == "" {
		host = req.URL.Hostname()
	}
	if f.calls != nil {
		*f.calls = append(*f.calls, fakeCall{req.URL.Hostname(), host, SNIHostname(req.Context()), req.URL.Path})
	}
	action, ok := f.behavior[req.URL.Hostname()]
	f.mu.Unlock()
	if !ok {
		action = "ok"
	}
	switch a := action.(type) {
	case string:
		switch a {
		case "timeout":
			return nil, &ConnectError{Err: errors.New("timed out"), Timeout: true}
		case "connect_error":
			return nil, &ConnectError{Err: errors.New("connect error")}
		}
	case error:
		return nil, a
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Request: req}, nil
}

func (f *fakeNetTransport) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	if f.closeLog != nil {
		*f.closeLog = append(*f.closeLog, f)
	}
	return nil
}

type fakeNetFactory struct {
	mu        sync.Mutex
	calls     []fakeCall
	behavior  map[string]any
	instances []*fakeNetTransport
	closed    []*fakeNetTransport
	seen      []TransportOptions
}

func (f *fakeNetFactory) build(opts TransportOptions) (InnerTransport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeNetTransport{mu: &f.mu, calls: &f.calls, behavior: f.behavior, closeLog: &f.closed}
	f.instances = append(f.instances, t)
	f.seen = append(f.seen, opts)
	return t, nil
}

func (f *fakeNetFactory) hosts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.urlHost)
	}
	return out
}

func clearProxyEnv(t *testing.T) {
	for _, key := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy", "http_proxy", "all_proxy", "TELEGRAM_PROXY", "NO_PROXY", "no_proxy"} {
		t.Setenv(key, "")
	}
}

func newFakeFallback(t *testing.T, ips []string, behavior map[string]any) (*TelegramFallbackTransport, *fakeNetFactory) {
	t.Helper()
	clearProxyEnv(t)
	f := &fakeNetFactory{behavior: behavior}
	tr, err := NewTelegramFallbackTransport(context.Background(), ips, TransportOptions{Factory: f.build})
	if err != nil {
		t.Fatal(err)
	}
	return tr, f
}

func telegramRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://api.telegram.org/botTOKEN/getMe", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func sticky(tr *TelegramFallbackTransport) (bool, string) {
	tr.stickyMu.Lock()
	defer tr.stickyMu.Unlock()
	return tr.stickySet, tr.stickyIP
}

func TestFiltersInvalidAndIpv6(t *testing.T) {
	got := ParseFallbackIPEnv("149.154.167.220, bad, 2001:67c:4e8:f004::9,149.154.167.220")
	if !slices.Equal(got, []string{"149.154.167.220", "149.154.167.220"}) {
		t.Fatalf("ips %v", got)
	}
	if got := ParseFallbackIPEnv("10.0.0.1,127.0.0.1,169.254.1.1,0.0.0.0,192.168.1.1,149.154.166.110"); !slices.Equal(got, []string{"149.154.166.110"}) {
		t.Fatalf("internal addresses kept %v", got)
	}
}

func TestNoneReturnsEmpty(t *testing.T) {
	if got := ParseFallbackIPEnv(""); len(got) != 0 {
		t.Fatalf("ips %v", got)
	}
}

func TestPreservesHostAndSni(t *testing.T) {
	rewritten := rewriteRequestForIP(telegramRequest(t), "149.154.167.220")
	if rewritten.URL.Hostname() != "149.154.167.220" || rewritten.Host != "api.telegram.org" || SNIHostname(rewritten.Context()) != "api.telegram.org" || rewritten.URL.Path != "/botTOKEN/getMe" {
		t.Fatalf("rewritten %v host %q sni %q", rewritten.URL, rewritten.Host, SNIHostname(rewritten.Context()))
	}
}

func TestIpv4LiteralTriedBeforeHostnameAndBecomesSticky(t *testing.T) {
	tr, f := newFakeFallback(t, []string{"149.154.167.220"}, map[string]any{"api.telegram.org": "timeout", "149.154.167.220": "ok"})
	resp, err := tr.RoundTrip(telegramRequest(t))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if set, ip := sticky(tr); !set || ip != "149.154.167.220" {
		t.Fatalf("sticky %v %q", set, ip)
	}
	if !slices.Equal(f.hosts(), []string{"149.154.167.220"}) || f.calls[0].hostHeader != "api.telegram.org" || f.calls[0].sni != "api.telegram.org" {
		t.Fatalf("calls %+v", f.calls)
	}
	f.calls = nil
	if resp, err := tr.RoundTrip(telegramRequest(t)); err != nil || resp.StatusCode != 200 || f.calls[0].urlHost != "149.154.167.220" {
		t.Fatalf("second resp %v err %v calls %+v", resp, err, f.calls)
	}
}

func TestStickyIPTriedFirstButFallsThroughIfStale(t *testing.T) {
	behavior := map[string]any{"api.telegram.org": "timeout", "149.154.167.220": "ok", "149.154.167.221": "ok"}
	tr, f := newFakeFallback(t, []string{"149.154.167.220", "149.154.167.221"}, behavior)
	if _, err := tr.RoundTrip(telegramRequest(t)); err != nil {
		t.Fatal(err)
	}
	if _, ip := sticky(tr); ip != "149.154.167.220" {
		t.Fatalf("sticky %q", ip)
	}
	f.mu.Lock()
	f.calls = nil
	behavior["149.154.167.220"] = "timeout"
	f.mu.Unlock()
	resp, err := tr.RoundTrip(telegramRequest(t))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if !slices.Equal(f.hosts(), []string{"149.154.167.220", "149.154.167.221"}) {
		t.Fatalf("calls %v", f.hosts())
	}
	if _, ip := sticky(tr); ip != "149.154.167.221" {
		t.Fatalf("sticky %q", ip)
	}
}

func TestHostnameTriedLastWhenIpv4Fails(t *testing.T) {
	tr, f := newFakeFallback(t, []string{"149.154.167.220"}, map[string]any{"149.154.167.220": "timeout", "api.telegram.org": "ok"})
	resp, err := tr.RoundTrip(telegramRequest(t))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if !slices.Equal(f.hosts(), []string{"149.154.167.220", "api.telegram.org"}) {
		t.Fatalf("calls %v", f.hosts())
	}
	if set, ip := sticky(tr); !set || ip != "" {
		t.Fatalf("sticky %v %q, want the hostname", set, ip)
	}
}

func TestNonTelegramHostBypassesFallback(t *testing.T) {
	tr, f := newFakeFallback(t, []string{"149.154.167.220"}, map[string]any{})
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/path", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil || resp.StatusCode != 200 || f.calls[0].urlHost != "example.com" {
		t.Fatalf("resp %v err %v calls %+v", resp, err, f.calls)
	}
	if set, _ := sticky(tr); set {
		t.Fatal("bypass set a sticky path")
	}
}

func TestUsesProxyEnvForPrimaryAndFallbackTransports(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://proxy.example:8080")
	f := &fakeNetFactory{behavior: map[string]any{}}
	tr, err := NewTelegramFallbackTransport(context.Background(), []string{"149.154.167.220"}, TransportOptions{Factory: f.build})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tr.FallbackIPs(), []string{"149.154.167.220"}) {
		t.Fatalf("ips %v", tr.FallbackIPs())
	}
	if _, err := tr.getFallback("149.154.167.220"); err != nil {
		t.Fatal(err)
	}
	if len(f.seen) != 2 {
		t.Fatalf("built %d transports", len(f.seen))
	}
	for _, o := range f.seen {
		if o.Proxy == nil || *o.Proxy != "http://proxy.example:8080" {
			t.Fatalf("proxy %v", o.Proxy)
		}
	}
}

func TestNoProxyBypassesFallbackIPCidr(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://proxy.example:8080")
	t.Setenv("NO_PROXY", "149.154.160.0/20")
	f := &fakeNetFactory{behavior: map[string]any{}}
	tr, err := NewTelegramFallbackTransport(context.Background(), []string{"149.154.167.220"}, TransportOptions{Factory: f.build})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.getFallback("149.154.167.220"); err != nil {
		t.Fatal(err)
	}
	if len(f.seen) != 2 {
		t.Fatalf("built %d transports", len(f.seen))
	}
	for _, o := range f.seen {
		if o.Proxy != nil {
			t.Fatalf("proxy %q passed despite NO_PROXY", *o.Proxy)
		}
	}
}

func TestForwardsLimitsToInnerTransports(t *testing.T) {
	clearProxyEnv(t)
	custom := &PoolLimits{MaxConnections: 42, MaxKeepaliveConnections: 10, KeepaliveExpiry: 30 * time.Second}
	f := &fakeNetFactory{behavior: map[string]any{}}
	tr, err := NewTelegramFallbackTransport(context.Background(), []string{"149.154.167.220"}, TransportOptions{Factory: f.build, Limits: custom})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.getFallback("149.154.167.220"); err != nil {
		t.Fatal(err)
	}
	if len(f.seen) != 2 {
		t.Fatalf("built %d transports", len(f.seen))
	}
	for _, o := range f.seen {
		if o.Limits != custom || o.SocketOptions == nil || !o.SocketOptions.Enable {
			t.Fatalf("options %+v", o)
		}
	}
}

func TestAcloseClosesAllTransports(t *testing.T) {
	tr, f := newFakeFallback(t, []string{"149.154.167.220", "149.154.167.221"}, map[string]any{})
	for _, ip := range []string{"149.154.167.220", "149.154.167.221"} {
		if _, err := tr.getFallback(ip); err != nil {
			t.Fatal(err)
		}
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if len(f.instances) != 3 {
		t.Fatalf("instances %d", len(f.instances))
	}
	for _, i := range f.instances {
		if !i.closed {
			t.Fatal("transport left open")
		}
	}
}

func TestFailedFallbackPoolIsDiscardedAndClosed(t *testing.T) {
	behavior := map[string]any{"api.telegram.org": "timeout", "149.154.167.220": "connect_error", "149.154.167.221": "timeout"}
	tr, f := newFakeFallback(t, []string{"149.154.167.220", "149.154.167.221"}, behavior)
	_, err := tr.RoundTrip(telegramRequest(t))
	var ce *ConnectError
	if !errors.As(err, &ce) {
		t.Fatalf("err %v", err)
	}
	if len(tr.fallbacks) != 0 {
		t.Fatalf("failed pools retained %v", tr.fallbacks)
	}
	if len(f.closed) != 3 {
		t.Fatalf("closed %d transports", len(f.closed))
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFailedPrimaryPoolIsDiscardedAndClosed(t *testing.T) {
	tr, f := newFakeFallback(t, []string{"149.154.167.220"}, map[string]any{"api.telegram.org": "timeout", "149.154.167.220": "ok"})
	defer tr.Close()
	resp, err := tr.RoundTrip(telegramRequest(t))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if len(f.instances) != 2 || f.instances[0].closed || f.instances[1].closed {
		t.Fatalf("instances %d", len(f.instances))
	}
	if _, ip := sticky(tr); ip != "149.154.167.220" {
		t.Fatalf("sticky %q", ip)
	}
}

func TestPoolDefaultLimitsAppliedWhenCallerOmits(t *testing.T) {
	_, f := newFakeFallback(t, []string{"149.154.167.220"}, map[string]any{})
	if len(f.seen) != 1 || f.seen[0].Limits == nil || *f.seen[0].Limits != FallbackPoolLimits || FallbackPoolLimits.MaxConnections != 8 {
		t.Fatalf("limits %+v", f.seen)
	}
}

func TestNonConnectErrorIsRaisedWithoutFallthrough(t *testing.T) {
	boom := errors.New("read reset")
	tr, f := newFakeFallback(t, []string{"149.154.167.220"}, map[string]any{"149.154.167.220": boom})
	if _, err := tr.RoundTrip(telegramRequest(t)); !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	if !slices.Equal(f.hosts(), []string{"149.154.167.220"}) {
		t.Fatalf("calls %v", f.hosts())
	}
}

type dohRoute struct {
	status int
	body   string
	err    error
}

type fakeDoH struct {
	mu       sync.Mutex
	routes   map[string]dohRoute
	requests []*http.Request
}

func (f *fakeDoH) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	for prefix, r := range f.routes {
		if strings.HasPrefix(req.URL.String(), prefix) {
			if r.err != nil {
				return nil, r.err
			}
			return &http.Response{StatusCode: r.status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(r.body)), Request: req}, nil
		}
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
}

func dohAnswer(ips ...string) string {
	parts := make([]string, len(ips))
	for i, ip := range ips {
		parts[i] = `{"type": 1, "data": "` + ip + `"}`
	}
	return `{"Answer": [` + strings.Join(parts, ", ") + `]}`
}

func discoverWith(routes map[string]dohRoute, system func() []string) ([]string, *fakeDoH) {
	f := &fakeDoH{routes: routes}
	return discoverFallbackIPs(context.Background(), &http.Client{Transport: f}, system), f
}

func systemIPs(ips ...string) func() []string { return func() []string { return ips } }

func TestGoogleAndCloudflareIpsCollected(t *testing.T) {
	ips, f := discoverWith(map[string]dohRoute{
		"https://dns.google":         {200, dohAnswer("149.154.167.220"), nil},
		"https://cloudflare-dns.com": {200, dohAnswer("149.154.167.221"), nil},
	}, systemIPs("149.154.166.110"))
	if !slices.Contains(ips, "149.154.167.220") || !slices.Contains(ips, "149.154.167.221") {
		t.Fatalf("ips %v", ips)
	}
	for _, r := range f.requests {
		if r.URL.Query().Get("name") != TelegramAPIHost || r.URL.Query().Get("type") != "A" {
			t.Fatalf("query %v", r.URL)
		}
		if strings.HasPrefix(r.URL.String(), "https://cloudflare-dns.com") && r.Header.Get("Accept") != "application/dns-json" {
			t.Fatalf("cloudflare headers %v", r.Header)
		}
	}
}

func TestSystemDNSIPKeptWhenDohConfirms(t *testing.T) {
	ips, _ := discoverWith(map[string]dohRoute{
		"https://dns.google":         {200, dohAnswer("149.154.166.110", "149.154.167.220"), nil},
		"https://cloudflare-dns.com": {200, dohAnswer("149.154.166.110"), nil},
	}, systemIPs("149.154.166.110"))
	if !slices.Equal(ips, []string{"149.154.166.110", "149.154.167.220"}) {
		t.Fatalf("ips %v", ips)
	}
}

func TestDohResultsDeduplicated(t *testing.T) {
	ips, _ := discoverWith(map[string]dohRoute{
		"https://dns.google":         {200, dohAnswer("149.154.167.220"), nil},
		"https://cloudflare-dns.com": {200, dohAnswer("149.154.167.220"), nil},
	}, systemIPs("149.154.166.110"))
	if !slices.Equal(ips, []string{"149.154.167.220"}) {
		t.Fatalf("ips %v", ips)
	}
}

func TestAllDohIpsSameAsSystemDNSKept(t *testing.T) {
	ips, _ := discoverWith(map[string]dohRoute{
		"https://dns.google":         {200, dohAnswer("149.154.166.110"), nil},
		"https://cloudflare-dns.com": {200, dohAnswer("149.154.166.110"), nil},
	}, systemIPs("149.154.166.110"))
	if !slices.Equal(ips, []string{"149.154.166.110"}) {
		t.Fatalf("ips %v", ips)
	}
}

func TestHungSystemDNSDoesNotGateDohResults(t *testing.T) {
	saved := dohTimeout
	dohTimeout = 200 * time.Millisecond
	defer func() { dohTimeout = saved }()
	release := make(chan struct{})
	defer close(release)
	start := time.Now()
	ips, _ := discoverWith(map[string]dohRoute{
		"https://dns.google":         {200, dohAnswer("149.154.167.220"), nil},
		"https://cloudflare-dns.com": {200, dohAnswer(), nil},
	}, func() []string { <-release; return []string{"149.154.166.110"} })
	if !slices.Equal(ips, []string{"149.154.167.220"}) {
		t.Fatalf("ips %v", ips)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("discovery gated by the hung resolver for %v", elapsed)
	}
}

func TestDohFailuresFallBackToSeedIps(t *testing.T) {
	ips, _ := discoverWith(map[string]dohRoute{
		"https://dns.google":         {500, `{}`, nil},
		"https://cloudflare-dns.com": {200, `{"Answer": [{"type": 1, "data": 5}, {"type": 1, "data": "149.154.167.9"}]}`, nil},
	}, systemIPs())
	if !slices.Equal(ips, SeedFallbackIPs) {
		t.Fatalf("ips %v", ips)
	}
	ips, _ = discoverWith(map[string]dohRoute{
		"https://dns.google":         {200, `{"Answer": [{"type": 5, "data": "149.154.167.1"}, {"type": 1.0, "data": " not-an-ip "}, {"type": true, "data": " 149.154.167.2 "}]}`, nil},
		"https://cloudflare-dns.com": {0, "", errors.New("blocked")},
	}, systemIPs())
	if !slices.Equal(ips, []string{"149.154.167.2"}) {
		t.Fatalf("ips %v", ips)
	}
}

func TestDohClientSharesThePlatformSSLContext(t *testing.T) {
	client := newDoHClient()
	tr, ok := client.Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig != provider.PlatformTLSConfig() {
		t.Fatalf("DoH client TLS config %p, want the shared platform context", tr.TLSClientConfig)
	}
	if client.CheckRedirect == nil {
		t.Fatal("DoH client follows redirects")
	}
}
