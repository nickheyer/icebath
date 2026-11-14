package provider

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Clears every proxy variable and the shared transport pool for the test
func keepaliveNoProxyEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy", "http_proxy", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
	}
	closeSharedTransports()
	t.Cleanup(func() { closeSharedTransports() })
}

func keepaliveMounts(t *testing.T, c *http.Client) *keepaliveClientTransport {
	t.Helper()
	if c == nil {
		t.Fatal("keepalive client is nil")
	}
	ct, ok := c.Transport.(*keepaliveClientTransport)
	if !ok {
		t.Fatalf("transport is %T", c.Transport)
	}
	return ct
}

// Shared transport behind one scheme mount of a keepalive client
func keepaliveInner(t *testing.T, c *http.Client, scheme string) *keepaliveTransport {
	t.Helper()
	v, ok := keepaliveMounts(t, c).mounts[scheme].(*sharedTransportView)
	if !ok {
		t.Fatalf("%s mount is %T", scheme, keepaliveMounts(t, c).mounts[scheme])
	}
	return v.inner
}

func keepaliveGet(t *testing.T, c *http.Client, url string) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	return c.Do(req)
}

func keepaliveOK(t *testing.T, c *http.Client, url string) {
	t.Helper()
	resp, err := keepaliveGet(t, c, url)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func keepaliveServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "2")
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestSameIdentityClientsShareTransportButNotClient(t *testing.T) {
	keepaliveNoProxyEnv(t)
	a := buildKeepaliveHTTPClient("https://api.example.com/v1", false, TLSVerify{})
	b := buildKeepaliveHTTPClient("https://api.example.com/v1", false, TLSVerify{})
	if a == b || keepaliveMounts(t, a) == keepaliveMounts(t, b) {
		t.Fatal("each call must return a new client")
	}
	if keepaliveInner(t, a, "https") != keepaliveInner(t, b, "https") || keepaliveInner(t, a, "http") != keepaliveInner(t, b, "http") {
		t.Fatal("same identity clients must share the transport")
	}
	if keepaliveMounts(t, a).mounts["https"] == keepaliveMounts(t, b).mounts["https"] {
		t.Fatal("each client must own its transport view")
	}
	closeHTTPClient(a)
	closeHTTPClient(b)
}

func TestClosingOneClientLeavesSiblingFunctional(t *testing.T) {
	keepaliveNoProxyEnv(t)
	url := keepaliveServer(t)
	a := buildKeepaliveHTTPClient(url, false, TLSVerify{})
	b := buildKeepaliveHTTPClient(url, false, TLSVerify{})
	if keepaliveInner(t, a, "http") != keepaliveInner(t, b, "http") {
		t.Fatal("expected one shared transport")
	}
	keepaliveOK(t, a, url+"/x")
	closeHTTPClient(a)
	if !keepaliveMounts(t, a).closed.Load() {
		t.Fatal("closed client must report closed")
	}
	keepaliveOK(t, b, url+"/y")
	c := buildKeepaliveHTTPClient(url, false, TLSVerify{})
	if keepaliveInner(t, c, "http") != keepaliveInner(t, b, "http") {
		t.Fatal("a new client must reuse the shared transport")
	}
	keepaliveOK(t, c, url+"/z")
	_, err := keepaliveGet(t, a, url+"/closed")
	var rt *AuxRuntimeError
	if !errors.As(err, &rt) || rt.Message != keepaliveClosedMessage {
		t.Fatalf("closed client error = %v", err)
	}
	closeHTTPClient(b)
	closeHTTPClient(c)
}

func TestPoolSurvivesClientClose(t *testing.T) {
	keepaliveNoProxyEnv(t)
	url := keepaliveServer(t)
	a := buildKeepaliveHTTPClient(url, false, TLSVerify{})
	keepaliveOK(t, a, url+"/warm")
	closeHTTPClient(a)
	b := buildKeepaliveHTTPClient(url, false, TLSVerify{})
	reused := false
	req, _ := http.NewRequest(http.MethodGet, url+"/again", nil)
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}))
	resp, err := b.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if !reused {
		t.Fatal("client close must not drain the shared pool")
	}
	closeHTTPClient(b)
}

func TestDifferentVerifyOrProxyGetDifferentTransports(t *testing.T) {
	keepaliveNoProxyEnv(t)
	base := "https://api.example.com/v1"
	def := buildKeepaliveHTTPClient(base, false, TLSVerify{})
	insecure := buildKeepaliveHTTPClient(base, false, TLSVerify{Insecure: true, Config: &tls.Config{InsecureSkipVerify: true}})
	ctx := &tls.Config{MinVersion: tls.VersionTLS12}
	withCtx := buildKeepaliveHTTPClient(base, false, TLSVerify{Config: ctx})
	withCtx2 := buildKeepaliveHTTPClient(base, false, TLSVerify{Config: ctx})
	codex := buildKeepaliveHTTPClient("https://chatgpt.com/backend-api/codex", false, TLSVerify{})
	if keepaliveInner(t, def, "https") == keepaliveInner(t, insecure, "https") || keepaliveInner(t, def, "https") == keepaliveInner(t, withCtx, "https") {
		t.Fatal("verify identity must split transports")
	}
	if keepaliveInner(t, withCtx, "https") != keepaliveInner(t, withCtx2, "https") {
		t.Fatal("one context must share one transport")
	}
	if keepaliveInner(t, withCtx, "https").transport.TLSClientConfig != ctx {
		t.Fatal("the transport must carry the caller context")
	}
	if !keepaliveInner(t, insecure, "https").transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("insecure transport must skip verification")
	}
	if keepaliveInner(t, codex, "https") == keepaliveInner(t, def, "https") || !keepaliveInner(t, codex, "https").dialer.racing.Load() {
		t.Fatal("codex transport must be its own racing transport")
	}
	for _, c := range []*http.Client{def, insecure, withCtx, withCtx2, codex} {
		closeHTTPClient(c)
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:3128")
	proxied := buildKeepaliveHTTPClient(base, false, TLSVerify{})
	for scheme, rt := range keepaliveMounts(t, proxied).mounts {
		tr, ok := rt.(*http.Transport)
		if !ok || tr.Proxy == nil {
			t.Fatalf("%s mount of a proxied client is %T", scheme, rt)
		}
	}
	closeHTTPClient(proxied)
}

func TestForceCloseOnlyTouchesOwningClientsInflightSockets(t *testing.T) {
	keepaliveNoProxyEnv(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stream" {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-release
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	a := buildKeepaliveHTTPClient(srv.URL, false, TLSVerify{})
	b := buildKeepaliveHTTPClient(srv.URL, false, TLSVerify{})
	keepaliveOK(t, b, srv.URL+"/warm")
	va := keepaliveMounts(t, a).mounts["http"].(*sharedTransportView)
	vb := keepaliveMounts(t, b).mounts["http"].(*sharedTransportView)
	if len(va.inflightConns()) != 0 || len(vb.inflightConns()) != 0 {
		t.Fatal("idle pooled connections belong to no request")
	}
	if set, ok := auxPoolConnSets.Load(va.inner.transport); !ok || len(set.snapshot()) == 0 {
		t.Fatal("the shared pool must hold the warm connection")
	}
	if len(auxPoolSockets(a)) != 0 || forceCloseTCPSockets(a) != 0 {
		t.Fatal("nothing of a is in flight so nothing of a may be touched")
	}
	keepaliveOK(t, b, srv.URL+"/again")
	resp, err := keepaliveGet(t, b, srv.URL+"/stream")
	if err != nil {
		t.Fatal(err)
	}
	if len(va.inflightConns()) != 0 || len(vb.inflightConns()) != 1 {
		t.Fatalf("in flight a=%d b=%d", len(va.inflightConns()), len(vb.inflightConns()))
	}
	if len(auxPoolSockets(a)) != 0 || len(auxPoolSockets(b)) != 1 {
		t.Fatalf("pool sockets a=%d b=%d", len(auxPoolSockets(a)), len(auxPoolSockets(b)))
	}
	close(release)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if len(vb.inflightConns()) != 0 {
		t.Fatal("a finished stream must release its connection")
	}
	closeHTTPClient(a)
	closeHTTPClient(b)
}

func TestSharedTransportCacheIsBounded(t *testing.T) {
	keepaliveNoProxyEnv(t)
	old := sharedTransportsMax
	sharedTransportsMax = 2
	defer func() { sharedTransportsMax = old }()
	base := "https://api.example.com/v1"
	clients := []*http.Client{
		buildKeepaliveHTTPClient(base, false, TLSVerify{Insecure: true, Config: &tls.Config{InsecureSkipVerify: true}}),
		buildKeepaliveHTTPClient(base, false, TLSVerify{}),
	}
	if len(sharedTransports) != 2 {
		t.Fatalf("pool size %d", len(sharedTransports))
	}
	ctx := &tls.Config{MinVersion: tls.VersionTLS12}
	extra := buildKeepaliveHTTPClient(base, false, TLSVerify{Config: ctx})
	if len(sharedTransports) != 2 || keepaliveInner(t, extra, "https").transport.TLSClientConfig != ctx {
		t.Fatal("past the cap a client gets its own transport")
	}
	for _, c := range append(clients, extra) {
		closeHTTPClient(c)
	}
	if n := closeSharedTransports(); n != 2 {
		t.Fatalf("closed %d", n)
	}
}

func TestCodexSyncClientUsesHappyEyeballsBackend(t *testing.T) {
	keepaliveNoProxyEnv(t)
	c := buildKeepaliveHTTPClient("https://chatgpt.com/backend-api/codex", false, TLSVerify{})
	for _, scheme := range []string{"http", "https"} {
		if !keepaliveInner(t, c, scheme).dialer.racing.Load() {
			t.Fatalf("%s transport must race", scheme)
		}
	}
}

func TestOtherSyncClientsKeepDefaultBackend(t *testing.T) {
	keepaliveNoProxyEnv(t)
	for _, base := range []string{"https://api.openai.com/v1", "https://chatgpt.com/backend-api/other"} {
		c := buildKeepaliveHTTPClient(base, false, TLSVerify{})
		if keepaliveInner(t, c, "https").dialer.racing.Load() {
			t.Fatalf("%s must connect serially", base)
		}
	}
}

func TestAsyncCodexClientRacesNatively(t *testing.T) {
	keepaliveNoProxyEnv(t)
	c := buildKeepaliveHTTPClient("https://chatgpt.com/backend-api/codex", true, TLSVerify{})
	ct := keepaliveMounts(t, c)
	if len(ct.private) != 2 || len(sharedTransports) != 0 {
		t.Fatal("async clients never share a transport")
	}
	for _, p := range ct.private {
		if !p.dialer.racing.Load() {
			t.Fatal("async transports race every connect")
		}
	}
}

// Black holed IPv6 first, the IPv4 attempt starts after the stagger and wins long before the timeout
func TestConnectionStaggersPastBlackholedIPv6(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	oldLookup, oldConnect := happyEyeballsLookupFn, happyEyeballsConnectFn
	defer func() { happyEyeballsLookupFn, happyEyeballsConnectFn = oldLookup, oldConnect }()
	happyEyeballsLookupFn = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("2001:db8::1"), net.ParseIP("2001:db8::2"), net.ParseIP("127.0.0.1")}, nil
	}
	var mu sync.Mutex
	var order []string
	happyEyeballsConnectFn = func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		order = append(order, address)
		mu.Unlock()
		if strings.HasPrefix(address, "[2001:db8") {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	start := time.Now()
	conn, err := happyEyeballsDial(context.Background(), "tcp", net.JoinHostPort("example.test", port), 10*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if elapsed < happyEyeballsDelay || elapsed > 3*time.Second {
		t.Fatalf("elapsed %v", elapsed)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) < 2 || !strings.HasPrefix(order[1], "127.0.0.1") {
		t.Fatalf("families must interleave, got %v", order)
	}
}

func TestInterleaveDialAddrsRoundRobinsFamilies(t *testing.T) {
	in := []net.IP{net.ParseIP("::1"), net.ParseIP("::2"), net.ParseIP("10.0.0.1"), net.ParseIP("::1"), net.ParseIP("10.0.0.2")}
	got := interleaveDialAddrs(in)
	want := []string{"::1", "10.0.0.1", "::2", "10.0.0.2"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("got %v", got)
		}
	}
}

func TestEnableHappyEyeballsOnClientSkipsProxyPools(t *testing.T) {
	keepaliveNoProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:3128")
	proxied := buildKeepaliveHTTPClient("https://api.example.com/v1", false, TLSVerify{})
	enableHappyEyeballsOnClient(proxied)
	for _, p := range keepaliveMounts(t, proxied).private {
		if !p.dialer.proxied || p.dialer.racing.Load() {
			t.Fatal("proxy pools keep their serial connect")
		}
	}
	var serialDials, racedLookups []string
	oldLookup := happyEyeballsLookupFn
	defer func() { happyEyeballsLookupFn = oldLookup }()
	happyEyeballsLookupFn = func(_ context.Context, host string) ([]net.IP, error) {
		racedLookups = append(racedLookups, host)
		return nil, errors.New("raced lookup")
	}
	proxyURL, err := url.Parse("http://127.0.0.1:3128")
	if err != nil {
		t.Fatal(err)
	}
	raw := &http.Transport{Proxy: http.ProxyURL(proxyURL), DialContext: func(_ context.Context, _, address string) (net.Conn, error) {
		serialDials = append(serialDials, address)
		return nil, errors.New("serial dial")
	}}
	enableHappyEyeballsOnClient(&http.Client{Transport: raw})
	req, err := http.NewRequest(http.MethodGet, "https://api.example.com/v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if target, err := raw.Proxy(req); err != nil || target.Host != "127.0.0.1:3128" {
		t.Fatalf("proxy lookup %v %v", target, err)
	}
	if _, err := raw.DialContext(context.Background(), "tcp", "127.0.0.1:3128"); err == nil || err.Error() != "serial dial" {
		t.Fatalf("a connect to the proxy must keep the transport's own dialer, got %v", err)
	}
	if _, err := raw.DialContext(context.Background(), "tcp", "bypassed.example.com:443"); err == nil || err.Error() != "raced lookup" {
		t.Fatalf("a direct connect of a proxy backed transport must race, got %v", err)
	}
	if !reflect.DeepEqual(serialDials, []string{"127.0.0.1:3128"}) || !reflect.DeepEqual(racedLookups, []string{"bypassed.example.com"}) {
		t.Fatalf("serial dials %v raced lookups %v", serialDials, racedLookups)
	}
	direct := &http.Transport{}
	enableHappyEyeballsOnClient(&http.Client{Transport: direct})
	if direct.DialContext == nil {
		t.Fatal("a direct transport must race")
	}
	t.Setenv("HTTPS_PROXY", "")
	plain := buildKeepaliveHTTPClient("https://api.example.com/v1", false, TLSVerify{})
	enableHappyEyeballsOnClient(plain)
	if !keepaliveInner(t, plain, "https").dialer.racing.Load() {
		t.Fatal("direct keepalive transports must race once enabled")
	}
}

func TestLLMAndAdapterPathsBypassTheSameEntries(t *testing.T) {
	cases := [][2]string{
		{"10.0.0.0/8", "10.1.2.3"}, {"*.internal", "svc.internal"}, {"*.slack.com", "slack.com"},
		{".slack.com", "slack.com"}, {"localhost,.corp.example", "llm.corp.example"}, {"api.example.com:8443", "api.example.com:8443"},
	}
	for _, tc := range cases {
		keepaliveNoProxyEnv(t)
		t.Setenv("HTTPS_PROXY", "http://proxy.corp:3128")
		t.Setenv("NO_PROXY", tc[0])
		if !ShouldBypassProxy([]string{tc[1]}, nil) {
			t.Fatalf("%v must bypass", tc)
		}
		if got := getProxyForBaseURL("https://" + tc[1] + "/v1"); got != "" {
			t.Fatalf("%v kept proxy %q", tc, got)
		}
	}
}

func TestNonMatchingHostKeepsTheProxyOnBothPaths(t *testing.T) {
	keepaliveNoProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://proxy.corp:3128")
	t.Setenv("NO_PROXY", "10.0.0.0/8,*.internal")
	if got := getProxyForBaseURL("https://api.openai.com/v1"); got != "http://proxy.corp:3128" {
		t.Fatalf("got %q", got)
	}
	value := "*.slack.com"
	if ShouldBypassProxy([]string{"notslack.com"}, &value) {
		t.Fatal("notslack.com is not under slack.com")
	}
}

func TestMalformedPortInBaseURLKeepsTheProxyInsteadOfRaising(t *testing.T) {
	keepaliveNoProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://proxy.corp:3128")
	t.Setenv("NO_PROXY", "other.example")
	for _, u := range []string{"http://host:notaport/v1", "http://host:99999/v1"} {
		if got := getProxyForBaseURL(u); got != "http://proxy.corp:3128" {
			t.Fatalf("%s got %q", u, got)
		}
	}
	t.Setenv("NO_PROXY", "host")
	if got := getProxyForBaseURL("http://host:notaport/v1"); got != "" {
		t.Fatalf("host still matched, got %q", got)
	}
}

func TestGetProxyFromEnvPrefersHTTPSThenHTTPThenAll(t *testing.T) {
	keepaliveNoProxyEnv(t)
	if got := getProxyFromEnv(); got != "" {
		t.Fatalf("got %q", got)
	}
	steps := [][3]string{{"ALL_PROXY", "http://all:1", "http://all:1"}, {"HTTP_PROXY", "http://http:2", "http://http:2"}, {"HTTPS_PROXY", "http://https:3", "http://https:3"}}
	for _, s := range steps {
		t.Setenv(s[0], s[1])
		if got := getProxyFromEnv(); got != s[2] {
			t.Fatalf("after %s got %q", s[0], got)
		}
	}
}

func TestGetProxyFromEnvNormalizesSocksAlias(t *testing.T) {
	keepaliveNoProxyEnv(t)
	t.Setenv("ALL_PROXY", "socks://127.0.0.1:1080/")
	if got := getProxyFromEnv(); got != "socks5://127.0.0.1:1080/" {
		t.Fatalf("got %q", got)
	}
}

func TestGetProxyForBaseURLRespectsNoProxy(t *testing.T) {
	keepaliveNoProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7897")
	t.Setenv("NO_PROXY", "internal.example.com")
	if got := getProxyForBaseURL("https://litellm.internal.example.com/v1"); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := getProxyForBaseURL("https://api.openai.com/v1"); got != "http://127.0.0.1:7897" {
		t.Fatalf("got %q", got)
	}
}

// Captures the request a keepalive client would put on the wire
type keepaliveRecorder struct{ req *http.Request }

func (r *keepaliveRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.req = req
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
}

func keepaliveSend(t *testing.T, base, path string, payload map[string]any) (*http.Request, []byte) {
	t.Helper()
	keepaliveNoProxyEnv(t)
	c := buildKeepaliveHTTPClient(base, false, TLSVerify{})
	rec := &keepaliveRecorder{}
	ct := keepaliveMounts(t, c)
	ct.mounts["https"] = rec
	raw, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(raw))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	body, _ := io.ReadAll(rec.req.Body)
	return rec.req, body
}

func TestOfficialCodexResponsesBodyIsZstdAndLossless(t *testing.T) {
	official := "https://chatgpt.com/backend-api/codex"
	payload := map[string]any{"model": "gpt-6.1-sol", "input": []any{map[string]any{"role": "user", "content": strings.Repeat("hi ", 500)}}}
	req, body := keepaliveSend(t, official, "/responses", payload)
	if req.Header.Get("Content-Encoding") != "zstd" || req.Header.Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Fatalf("headers %v len %d", req.Header, len(body))
	}
	dec, _ := zstd.NewReader(nil)
	plain, err := dec.DecodeAll(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(plain, &got)
	if !reflect.DeepEqual(got, jsonRoundTrip(payload)) {
		t.Fatalf("lossy compression %v", got)
	}
}

func TestOtherDestinationsStayRawJSON(t *testing.T) {
	for _, tc := range [][2]string{
		{"https://my-proxy.example.com/backend-api/codex", "/responses"},
		{"https://api.openai.com/v1", "/responses"},
		{"https://chatgpt.com/backend-api/codex", "/models"},
	} {
		payload := map[string]any{"model": "x", "input": "hello"}
		req, body := keepaliveSend(t, tc[0], tc[1], payload)
		if req.Header.Get("Content-Encoding") != "" {
			t.Fatalf("%v compressed", tc)
		}
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil || !reflect.DeepEqual(got, jsonRoundTrip(payload)) {
			t.Fatalf("%v body %s", tc, body)
		}
	}
}

func jsonRoundTrip(v any) map[string]any {
	raw, _ := json.Marshal(v)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func TestBuildKeepaliveHTTPClientForwardsVerifyContext(t *testing.T) {
	keepaliveNoProxyEnv(t)
	ctx := &tls.Config{MinVersion: tls.VersionTLS12}
	c := buildKeepaliveHTTPClient("https://ollama.example.com/v1", false, TLSVerify{Config: ctx})
	if keepaliveInner(t, c, "https").transport.TLSClientConfig != ctx {
		t.Fatal("verify context must reach the transport")
	}
}

func TestBuildKeepaliveHTTPClientSSLCertFileUsesSharedContext(t *testing.T) {
	keepaliveNoProxyEnv(t)
	c := buildKeepaliveHTTPClient("https://ollama.example.com/v1", false, TLSVerify{})
	if keepaliveInner(t, c, "https").transport.TLSClientConfig != PlatformTLSConfig() {
		t.Fatal("the default verify mode must use the shared platform context")
	}
}

func TestBuildKeepaliveHTTPClientSSLVerifyFalse(t *testing.T) {
	keepaliveNoProxyEnv(t)
	verify := ResolveTLSVerify(nil, "", false, "https://ollama.example.com/v1")
	c := buildKeepaliveHTTPClient("https://ollama.example.com/v1", false, verify)
	if !keepaliveInner(t, c, "https").transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("ssl_verify false must skip verification")
	}
}

func TestPinnedClientsShareTransportNotCloseState(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	pinned := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	for _, insecure := range []bool{false, true} {
		keepaliveNoProxyEnv(t)
		verify := TLSVerify{Config: pinned}
		if insecure {
			verify = TLSVerify{Insecure: true, Config: &tls.Config{InsecureSkipVerify: true}}
		}
		a := buildKeepaliveHTTPClient(srv.URL, false, verify)
		b := buildKeepaliveHTTPClient(srv.URL, false, verify)
		if keepaliveInner(t, a, "https") != keepaliveInner(t, b, "https") {
			t.Fatal("pinned clients must share the transport")
		}
		keepaliveOK(t, a, srv.URL)
		closeHTTPClient(a)
		keepaliveOK(t, b, srv.URL)
		closeHTTPClient(b)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestSafeWriterSwallowsBrokenPipe(t *testing.T) {
	n, err := safeWriter{inner: failingWriter{}}.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	var buf bytes.Buffer
	if n, err := (safeWriter{inner: &buf}).Write([]byte("hi")); err != nil || n != 2 || buf.String() != "hi" {
		t.Fatal("healthy writes pass through")
	}
	installSafeStdio()
}

func TestBuildKeepaliveHTTPClientIsTheKeepaliveBuilder(t *testing.T) {
	keepaliveNoProxyEnv(t)
	a := BuildKeepaliveHTTPClient("https://api.example.com/v1", false, TLSVerify{})
	b := buildKeepaliveHTTPClient("https://api.example.com/v1", false, TLSVerify{})
	if a == nil || keepaliveInner(t, a, "https") != keepaliveInner(t, b, "https") {
		t.Fatal("exported builder must hand back the shared keepalive transport")
	}
	closeHTTPClient(a)
	closeHTTPClient(b)
}

func TestInstallSafeStdioIgnoresBrokenPipeSignals(t *testing.T) {
	InstallSafeStdio()
	if !signal.Ignored(syscall.SIGPIPE) {
		t.Fatal("SIGPIPE is still delivered after InstallSafeStdio")
	}
}
