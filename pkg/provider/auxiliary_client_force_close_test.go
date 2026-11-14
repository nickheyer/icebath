package provider

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Socket double recording shutdown and close calls in order
type fcFakeConn struct {
	net.Conn
	mu    sync.Mutex
	calls []string
}

func (c *fcFakeConn) record(name string) {
	c.mu.Lock()
	c.calls = append(c.calls, name)
	c.mu.Unlock()
}

func (c *fcFakeConn) CloseRead() error  { c.record("shutdown_read"); return nil }
func (c *fcFakeConn) CloseWrite() error { c.record("shutdown_write"); return nil }
func (c *fcFakeConn) Close() error      { c.record("close"); return nil }

// Tracked transport client whose pool holds the given connections
func fcClientWithConns(conns ...net.Conn) *http.Client {
	t := auxTrackPoolConns(&http.Transport{})
	set, _ := auxPoolConnSets.Load(t)
	for _, c := range conns {
		set.add(c)
	}
	return &http.Client{Transport: t}
}

func TestForceCloseTCPSocketsShutdownOnlyNoClose(t *testing.T) {
	sock := &fcFakeConn{}
	if n := forceCloseTCPSockets(fcClientWithConns(sock)); n != 1 {
		t.Fatalf("count %d", n)
	}
	if len(sock.calls) != 2 || sock.calls[0] != "shutdown_read" || sock.calls[1] != "shutdown_write" {
		t.Fatalf("shutdown must run and close must not %v", sock.calls)
	}
}

func TestForceCloseTCPSocketsUsesShutRdwr(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	defer server.Close()
	if n := ForceCloseTCPSockets(fcClientWithConns(client)); n != 1 {
		t.Fatalf("count %d", n)
	}
	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := server.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("peer must see the write side shut %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("read side must be shut %v", err)
	}
	rc, err := client.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Control(func(uintptr) {}); err != nil {
		t.Fatalf("descriptor must stay open %v", err)
	}
}

func TestForceCloseTCPSocketsDescendsHttpcore1ConnectionWrapper(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	tr := auxTrackPoolConns(&http.Transport{TLSClientConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig})
	hc := &http.Client{Transport: tr}
	resp, err := hc.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	client := newAuxOpenAIClient("k", srv.URL, auxOpenAIOptions{HTTPClient: hc})
	socks := auxPoolSockets(client)
	if len(socks) != 1 {
		t.Fatalf("sockets %d", len(socks))
	}
	if _, ok := socks[0].(*net.TCPConn); !ok {
		t.Fatalf("the TLS and pool wrappers must be descended to the TCP socket, got %T", socks[0])
	}
	if n := forceCloseTCPSockets(client); n != 1 {
		t.Fatalf("count %d", n)
	}
}

func TestForceCloseTCPSocketsFindsSocketsOnHTTPXMounts(t *testing.T) {
	keepaliveNoProxyEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	client := buildKeepaliveHTTPClient(srv.URL, true, TLSVerify{})
	defer closeHTTPClient(client)
	keepaliveOK(t, client, srv.URL+"/warm")
	if n := forceCloseTCPSockets(client); n != 1 {
		t.Fatalf("the mounted pool connection must be found, count %d", n)
	}
}

// Server whose stream route holds the response open until released
func fcStreamServer(release chan struct{}) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stream" {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
}

func TestForceCloseTCPSocketsFindsInFlightPoolRequestSockets(t *testing.T) {
	keepaliveNoProxyEnv(t)
	release := make(chan struct{})
	srv := fcStreamServer(release)
	defer srv.Close()
	defer close(release)
	client := buildKeepaliveHTTPClient(srv.URL, false, TLSVerify{})
	defer closeHTTPClient(client)
	resp, err := keepaliveGet(t, client, srv.URL+"/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if n := forceCloseTCPSockets(client); n != 1 {
		t.Fatalf("the checked out connection must be found, count %d", n)
	}
}

func TestForceCloseTCPSocketsClearsTimeoutBeforeShutdown(t *testing.T) {
	keepaliveNoProxyEnv(t)
	release := make(chan struct{})
	srv := fcStreamServer(release)
	defer srv.Close()
	defer close(release)
	client := buildKeepaliveHTTPClient(srv.URL, false, TLSVerify{})
	defer closeHTTPClient(client)
	resp, err := keepaliveGet(t, client, srv.URL+"/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	done := make(chan error, 1)
	go func() {
		_, err := resp.Body.Read(make([]byte, 16))
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if n := forceCloseTCPSockets(client); n != 1 {
		t.Fatalf("count %d", n)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the hung read must fail once the socket is shut down")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a hung read must notice the shutdown")
	}
}

func TestPoolSocketsMatchesTheForceCloseSweep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	hc := &http.Client{Transport: auxTrackPoolConns(&http.Transport{})}
	resp, err := hc.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	socks := PoolSockets(hc)
	if len(socks) != 1 || len(auxPoolSockets(hc)) != 1 {
		t.Fatalf("sockets %d", len(socks))
	}
	if _, ok := socks[0].(*net.TCPConn); !ok {
		t.Fatalf("raw TCP socket expected, got %T", socks[0])
	}
	if PoolSockets(nil) != nil {
		t.Fatal("a nil client has no sockets")
	}
}

func TestTrackPoolConnsSeedsTheSweep(t *testing.T) {
	sock := &fcFakeConn{}
	client := &http.Client{Transport: TrackPoolConns(&http.Transport{}, sock)}
	if n := ForceCloseTCPSockets(client); n != 1 || len(sock.calls) != 2 || sock.calls[0] != "shutdown_read" || sock.calls[1] != "shutdown_write" {
		t.Fatalf("count %d calls %v", n, sock.calls)
	}
}
