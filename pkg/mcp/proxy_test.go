package mcp

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	proxyTestURL = "https://mcp.example.com/mcp"
	proxyTestURI = "http://127.0.0.1:10808"
)

// envOnlyProxy clears every proxy variable and hides the operating system proxy configuration
func envOnlyProxy(t *testing.T) {
	t.Helper()
	restoreEnviron(t)
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy", "NO_PROXY", "no_proxy", "REQUEST_METHOD"} {
		os.Unsetenv(k)
	}
	prevProxies, prevBypass := systemProxies, systemProxyBypass
	systemProxies = func() map[string]string { return map[string]string{} }
	systemProxyBypass = func(string) bool { return false }
	t.Cleanup(func() { systemProxies, systemProxyBypass = prevProxies, prevBypass })
}

// mountedProxy returns the proxy the owned client dials for target, failing unless the body cap wraps it
func mountedProxy(t *testing.T, oc *ownedClient, target string) *url.URL {
	t.Helper()
	capped, ok := oc.client.Transport.(*bodyCapTransport)
	if !ok || capped.limit != MaxHTTPBodyBytes {
		t.Fatalf("client transport %T is not the wire body cap", oc.client.Transport)
	}
	inner := capped.inner.(*http.Transport)
	req, _ := http.NewRequest(http.MethodGet, target, nil)
	u, err := inner.Proxy(req)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestProxyEnvBecomesAMountAndNoProxyStaysDirect(t *testing.T) {
	envOnlyProxy(t)
	if m, err := mcpProxyMounts(proxyTestURL); m != nil || err != nil {
		t.Fatalf("no proxy configured must connect directly, got %v %v", m, err)
	}
	os.Setenv("HTTPS_PROXY", proxyTestURI)
	m, err := mcpProxyMounts(proxyTestURL)
	if err != nil || len(m) != 1 || m["https"] == nil || m["https"].String() != proxyTestURI {
		t.Fatalf("https mount only, got %v %v", m, err)
	}
	oc, err := newOwnedClient(HTTPOptions{URL: proxyTestURL})
	if err != nil {
		t.Fatal(err)
	}
	if u := mountedProxy(t, oc, proxyTestURL); u == nil || u.String() != proxyTestURI {
		t.Fatalf("connect client proxy %v", u)
	}
	os.Setenv("HTTP_PROXY", proxyTestURI)
	if m, _ := mcpProxyMounts("http://127.0.0.1:5000/mcp"); m != nil {
		t.Fatalf("loopback dialed through proxy %v", m)
	}
	os.Setenv("NO_PROXY", "mcp.example.com")
	if m, _ := mcpProxyMounts(proxyTestURL); m != nil {
		t.Fatalf("NO_PROXY host dialed through proxy %v", m)
	}
	os.Setenv("NO_PROXY", "10.0.0.0/8")
	if m, _ := mcpProxyMounts("https://10.1.2.3/mcp"); m != nil {
		t.Fatalf("NO_PROXY CIDR host dialed through proxy %v", m)
	}
	if m, _ := mcpProxyMounts("https://11.1.2.3/mcp"); m == nil {
		t.Fatal("host outside the NO_PROXY CIDR must use the proxy")
	}
}

func TestProxyEnvironmentPrecedenceMatchesUrllib(t *testing.T) {
	envOnlyProxy(t)
	os.Setenv("HTTPS_PROXY", "http://upper:1")
	os.Setenv("https_proxy", "http://lower:2")
	os.Setenv("ALL_PROXY", "socks://all:3")
	m, err := mcpProxyMounts(proxyTestURL)
	if err != nil || m["https"].String() != "http://lower:2" || m["http"].String() != "socks5://all:3" {
		t.Fatalf("lowercase wins and all_proxy fills the gap normalized, got %v %v", m, err)
	}
	os.Setenv("http_proxy", "http://cgi:4")
	os.Setenv("REQUEST_METHOD", "GET")
	if m, _ := mcpProxyMounts(proxyTestURL); m["http"].String() != "http://cgi:4" {
		t.Fatalf("lowercase http_proxy survives the CGI rule, got %v", m)
	}
	os.Unsetenv("http_proxy")
	if m, _ := mcpProxyMounts(proxyTestURL); m["http"].String() != "socks5://all:3" {
		t.Fatalf("CGI drops HTTP_PROXY, got %v", m)
	}
	os.Setenv("https_proxy", "ftp://bad:5")
	if _, err := mcpProxyMounts(proxyTestURL); err == nil || !strings.Contains(err.Error(), "Unknown scheme for proxy URL") {
		t.Fatalf("unsupported proxy scheme must fail the connect, got %v", err)
	}
	if _, err := newOwnedClient(HTTPOptions{URL: proxyTestURL}); err == nil {
		t.Fatal("client build must surface the proxy error")
	}
	os.Setenv("https_proxy", "")
	os.Setenv("no_proxy", ".Example.COM")
	if !proxyBypass("mcp.example.com") || proxyBypass("example.org") {
		t.Fatal("stdlib bypass suffix match")
	}
}

// recordingProxy is a forward proxy that records each request line and Host header, answering 502
func recordingProxy(t *testing.T) (string, func() []string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []string
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil {
					return
				}
				mu.Lock()
				seen = append(seen, req.Method+" "+req.RequestURI+" x-test="+req.Header.Get("X-Test"))
				mu.Unlock()
				_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
			}(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return "http://" + ln.Addr().String(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestBothClientBuildersCarryProxyMountsNextToTheBodyCap(t *testing.T) {
	envOnlyProxy(t)
	proxy, seen := recordingProxy(t)
	os.Setenv("HTTPS_PROXY", proxy)
	os.Setenv("HTTP_PROXY", proxy)
	ht, err := StartStreamableHTTP(HTTPOptions{URL: proxyTestURL, ConnectTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer ht.Close()
	if u := mountedProxy(t, ht.oc, proxyTestURL); u == nil || u.String() != proxy {
		t.Fatalf("streamable HTTP proxy %v", u)
	}
	_ = ht.Send(context.Background(), &Message{JSONRPC: "2.0", ID: []byte("1"), Method: "initialize"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sse, err := StartSSE(ctx, HTTPOptions{URL: "http://mcp.example.com/sse", Headers: map[string]string{"X-Test": "1"}, ConnectTimeout: 5 * time.Second}, 5*time.Second)
	if err == nil {
		sse.Close()
		t.Fatal("SSE through a 502 proxy must fail")
	}
	got := strings.Join(seen(), "\n")
	if !strings.Contains(got, "CONNECT mcp.example.com:443") || !strings.Contains(got, "GET http://mcp.example.com/sse x-test=1") {
		t.Fatalf("both transports must dial through the proxy with headers intact, proxy saw\n%s", got)
	}
}

func TestPreflightProbeUsesTheSameProxyMountsAsTheConnectClient(t *testing.T) {
	envOnlyProxy(t)
	proxy, seen := recordingProxy(t)
	os.Setenv("HTTPS_PROXY", proxy)
	s := &serverTask{name: "remote"}
	_ = s.preflightContentType(context.Background(), proxyTestURL, map[string]string{}, true, nil, nil, 2*time.Second, false)
	if got := strings.Join(seen(), "\n"); !strings.Contains(got, "CONNECT mcp.example.com:443") {
		t.Fatalf("preflight must dial through the proxy mount, proxy saw\n%s", got)
	}
}
