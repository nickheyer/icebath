package pyrequests

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

var base = &http.Transport{}

func send(t *testing.T, target string, tm Timeout, tr *http.Transport) (*Response, *Error) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, target, strings.NewReader("{}"))
	resp, err := Send(context.Background(), tr, tm, req)
	if err == nil {
		return resp, nil
	}
	e, ok := err.(*Error)
	if !ok {
		t.Fatalf("not a requests error: %T %v", err, err)
	}
	return nil, e
}

func hijacker(t *testing.T, fn func(net.Conn)) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go fn(c)
		}
	}()
	return ln.Addr().String()
}

func TestConnectFailures(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	port := addr[strings.LastIndex(addr, ":")+1:]
	_, e := send(t, "http://"+addr+"/v1/search?x=1", Seconds(30), base)
	want := fmt.Sprintf(`HTTPConnectionPool(host='127.0.0.1', port=%s): Max retries exceeded with url: /v1/search?x=1 (Caused by NewConnectionError("HTTPConnection(host='127.0.0.1', port=%s): Failed to establish a new connection: [Errno 111] Connection refused"))`, port, port)
	if e == nil || e.Class != "ConnectionError" || e.Text != want {
		t.Fatalf("refused %+v", e)
	}
	_, e = send(t, "https://no-such-host.invalid/v1/search", Seconds(30), base)
	if e == nil || e.Class != "ConnectionError" || !e.DNS || !strings.HasPrefix(e.Text, `HTTPSConnectionPool(host='no-such-host.invalid', port=443): Max retries exceeded with url: /v1/search (Caused by NameResolutionError("HTTPSConnection(host='no-such-host.invalid', port=443): Failed to resolve 'no-such-host.invalid' ([Errno `) {
		t.Fatalf("dns %+v", e)
	}
	u, _ := url.Parse("https://h.example/p")
	te := transportError(u, nil, phaseConnect, Seconds(600.0), &net.OpError{Op: "dial", Err: timeoutErr{}})
	if te.Class != "ConnectTimeout" || !regexp.MustCompile(`^HTTPSConnectionPool\(host='h.example', port=443\): Max retries exceeded with url: /p \(Caused by ConnectTimeoutError\(<HTTPSConnection\(host='h.example', port=443\) at 0x[0-9a-f]+>, 'Connection to h.example timed out. \(connect timeout=600.0\)'\)\)$`).MatchString(te.Text) {
		t.Fatalf("connect timeout %+v", te)
	}
	proxy, _ := url.Parse("http://proxy.local:3128")
	pe := transportError(u, proxy, phaseConnect, Seconds(30), &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)})
	if pe.Class != "ProxyError" || !strings.Contains(pe.Text, `(Caused by ProxyError('Unable to connect to proxy', NewConnectionError("HTTPConnection(host='proxy.local', port=3128): Failed to establish a new connection: [Errno 111] Connection refused")))`) || !strings.HasPrefix(pe.Text, "HTTPSConnectionPool(host='h.example', port=443): Max retries exceeded with url: /p ") {
		t.Fatalf("proxy %+v", pe)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestExchangeFailures(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(600 * time.Millisecond) }))
	defer slow.Close()
	tm := Timeout{Connect: time.Second, Read: 200 * time.Millisecond, ConnectText: "30", ReadText: "30"}
	host := strings.TrimPrefix(slow.URL, "http://")
	port := host[strings.LastIndex(host, ":")+1:]
	if _, e := send(t, slow.URL+"/v1/search", tm, base); e == nil || e.Class != "ReadTimeout" || e.Text != "HTTPConnectionPool(host='127.0.0.1', port="+port+"): Read timed out. (read timeout=30)" {
		t.Fatalf("header wait %+v", e)
	}
	partial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{"))
		w.(http.Flusher).Flush()
		time.Sleep(600 * time.Millisecond)
	}))
	defer partial.Close()
	if _, e := send(t, partial.URL, tm, base); e == nil || e.Class != "ConnectionError" || !strings.HasSuffix(e.Text, "): Read timed out.") {
		t.Fatalf("body wait %+v", e)
	}
	reset := hijacker(t, func(c net.Conn) {
		c.Read(make([]byte, 4096))
		c.(*net.TCPConn).SetLinger(0)
		c.Close()
	})
	if _, e := send(t, "http://"+reset+"/", tm, base); e == nil || e.Text != "('Connection aborted.', ConnectionResetError(104, 'Connection reset by peer'))" {
		t.Fatalf("reset %+v", e)
	}
	closer := hijacker(t, func(c net.Conn) {
		c.Read(make([]byte, 4096))
		c.Close()
	})
	if _, e := send(t, "http://"+closer+"/", tm, base); e == nil || e.Text != "('Connection aborted.', RemoteDisconnected('Remote end closed connection without response'))" {
		t.Fatalf("disconnect %+v", e)
	}
	short := hijacker(t, func(c net.Conn) {
		c.Read(make([]byte, 4096))
		c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n0123456789"))
		c.Close()
	})
	if _, e := send(t, "http://"+short+"/", tm, base); e == nil || e.Class != "ChunkedEncodingError" || e.Text != "('Connection broken: IncompleteRead(10 bytes read, 90 more expected)', IncompleteRead(10 bytes read, 90 more expected))" {
		t.Fatalf("incomplete %+v", e)
	}
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201); w.Write([]byte("done")) }))
	defer ok.Close()
	if resp, e := send(t, ok.URL, tm, base); e != nil || resp.Status != 201 || string(resp.Body) != "done" {
		t.Fatalf("ok %+v %+v", resp, e)
	}
}

func TestTLSFailures(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	port := host[strings.LastIndex(host, ":")+1:]
	_, e := send(t, srv.URL+"/x", Seconds(30), base)
	if e == nil || e.Class != "SSLError" || e.Text != "HTTPSConnectionPool(host='127.0.0.1', port="+port+"): Max retries exceeded with url: /x (Caused by SSLError(SSLCertVerificationError(1, '[SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: self-signed certificate (_ssl.c:1082)')))" {
		t.Fatalf("self signed %+v", e)
	}
	trusted := srv.Client().Transport.(*http.Transport).Clone()
	trusted.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, host)
	}
	_, e = send(t, "https://localhost:"+port+"/", Seconds(30), trusted)
	if e == nil || !strings.Contains(e.Text, `SSLCertVerificationError(1, "[SSL: CERTIFICATE_VERIFY_FAILED] certificate verify failed: Hostname mismatch, certificate is not valid for 'localhost'. (_ssl.c:1082)")`) {
		t.Fatalf("hostname %+v", e)
	}
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer plain.Close()
	_, e = send(t, strings.Replace(plain.URL, "http://", "https://", 1), Seconds(30), base)
	if e == nil || !strings.HasSuffix(e.Text, "(Caused by SSLError(SSLError(1, '[SSL: WRONG_VERSION_NUMBER] wrong version number (_ssl.c:1082)')))") {
		t.Fatalf("wrong version %+v", e)
	}
	eof := hijacker(t, func(c net.Conn) {
		c.Read(make([]byte, 4096))
		c.Close()
	})
	_, e = send(t, "https://"+eof+"/", Seconds(30), base)
	if e == nil || !strings.HasSuffix(e.Text, "(Caused by SSLError(SSLEOFError(8, '[SSL: UNEXPECTED_EOF_WHILE_READING] EOF occurred in violation of protocol (_ssl.c:1082)')))") {
		t.Fatalf("eof %+v", e)
	}
	alert := hijacker(t, func(c net.Conn) {
		c.Read(make([]byte, 4096))
		c.Write([]byte{21, 3, 3, 0, 2, 2, 40})
		c.Close()
	})
	_, e = send(t, "https://"+alert+"/", Seconds(30), base)
	if e == nil || !strings.HasSuffix(e.Text, "(Caused by SSLError(SSLError(1, '[SSL: SSLV3_ALERT_HANDSHAKE_FAILURE] ssl/tls alert handshake failure (_ssl.c:1082)')))") {
		t.Fatalf("alert %+v", e)
	}
	_ = tls.VersionTLS12
}

func TestPathURLAndSeconds(t *testing.T) {
	for raw, want := range map[string]string{"https://h": "/", "https://h/a%20b?q=1&r=2": "/a%20b?q=1&r=2"} {
		u, _ := url.Parse(raw)
		if got := PathURL(u); got != want {
			t.Fatalf("%s: %s", raw, got)
		}
	}
	if s := Seconds(30); s.ReadText != "30" || s.Connect != 30*time.Second {
		t.Fatalf("%+v", s)
	}
	if s := Seconds(600.0); s.ConnectText != "600.0" || s.Read != 600*time.Second {
		t.Fatalf("%+v", s)
	}
}
