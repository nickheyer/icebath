package remote

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Loopback listener whose every connection is handed to handle, returning its host and port
func rawServer(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handle(conn)
		}
	}()
	return ln.Addr().String()
}

// Reads the request head then writes reply and runs after before the connection closes
func answering(reply string, after func()) func(net.Conn) {
	return func(conn net.Conn) {
		defer conn.Close()
		buf := make([]byte, 65536)
		conn.Read(buf)
		conn.Write([]byte(reply))
		if after != nil {
			after()
		}
	}
}

// One GET against the plane at base with a short request bound, returning the error only
func requestError(t *testing.T, base string) error {
	t.Helper()
	t.Setenv(URLEnv, base)
	prev := httpTimeout
	httpTimeout = 400 * time.Millisecond
	t.Cleanup(func() { httpTimeout = prev })
	_, err := Request(context.Background(), "GET", t.TempDir(), "p", "", nil, "tok")
	return err
}

func TestBaseURLAndInstanceIDStripLikePythonStr(t *testing.T) {
	t.Setenv(URLEnv, " \x1c\thttps://plane.test/x//\x1d ")
	if got := BaseURL(); got != "https://plane.test/x" {
		t.Fatalf("base url %q", got)
	}
	t.Setenv(URLEnv, "\x1f \x1e")
	if got := BaseURL(); got != DefaultURL {
		t.Fatalf("a blank setting falls back to the default, got %q", got)
	}
	t.Setenv(InstanceEnv, "\x1einst-1\x1f\n")
	if got := InstanceID(); got != "inst-1" {
		t.Fatalf("instance id %q", got)
	}
}

func TestRetryAfterParsesLikePythonFloat(t *testing.T) {
	for text, want := range map[string]float64{"3": 3, " 2.5 ": 2.5, "1_0": 10, "١٢": 12, "1e3": 1000, "-0.5": -0.5,
		"1e999": math.Inf(1), "inf": math.Inf(1), "-Infinity": math.Inf(-1), ".5": 0.5, "5.": 5} {
		if got, ok := parseRetryAfter(text); !ok || got != want {
			t.Fatalf("%q parsed as %v %v", text, got, ok)
		}
	}
	if got, ok := parseRetryAfter("nan"); !ok || !math.IsNaN(got) {
		t.Fatalf("nan parsed as %v %v", got, ok)
	}
	for _, text := range []string{"", "soon", "0x10", "1__0", "_1", "1_", "1 0", "Wed, 21 Oct 2026 07:28:00 GMT", "1,5"} {
		if got, ok := parseRetryAfter(text); ok {
			t.Fatalf("%q must not parse, got %v", text, got)
		}
	}
}

func TestTransportFailuresCarryThePythonReasonTexts(t *testing.T) {
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	wait := func() { <-stall }
	silent := rawServer(t, func(conn net.Conn) {
		defer conn.Close()
		<-stall
	})
	var te *TransportError
	err := requestError(t, "http://"+silent)
	if want := "GET http://" + silent + "/v1/config/self?profile=p: timed out"; !errors.As(err, &te) || te.Message != want {
		t.Fatalf("silent plain server: %v", err)
	}
	err = requestError(t, "https://"+silent)
	if want := "GET https://" + silent + "/v1/config/self?profile=p: _ssl.c:1064: The handshake operation timed out"; !errors.As(err, &te) || te.Message != want {
		t.Fatalf("silent tls server: %v", err)
	}
	stalled := rawServer(t, answering("HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nhello", wait))
	err = requestError(t, "http://"+stalled)
	if want := "GET http://" + stalled + "/v1/config/self?profile=p: timed out"; !errors.As(err, &te) || te.Message != want {
		t.Fatalf("stalled body: %v", err)
	}
	closed := rawServer(t, answering("", nil))
	err = requestError(t, "http://"+closed)
	if want := "GET http://" + closed + "/v1/config/self?profile=p: Remote end closed connection without response"; !errors.As(err, &te) || te.Message != want {
		t.Fatalf("closed without an answer: %v", err)
	}
}

func TestShortBodyIsAnIncompleteReadAndNoTransportError(t *testing.T) {
	var te *TransportError
	var incomplete *IncompleteRead
	short := rawServer(t, answering("HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nhello", nil))
	err := requestError(t, "http://"+short)
	if !errors.As(err, &incomplete) || errors.As(err, &te) || err.Error() != "IncompleteRead(5 bytes read, 5 more expected)" {
		t.Fatalf("short body: %T %v", err, err)
	}
	chunked := rawServer(t, answering("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n", nil))
	err = requestError(t, "http://"+chunked)
	if !errors.As(err, &incomplete) || errors.As(err, &te) || err.Error() != "IncompleteRead(5 bytes read)" {
		t.Fatalf("short chunked body: %T %v", err, err)
	}
}

func TestStalledTLSBodyReportsTheReadOperationTimeout(t *testing.T) {
	stall := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		w.Write([]byte("hello"))
		w.(http.Flusher).Flush()
		<-stall
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stall) })
	prev := credentialedClient
	credentialedClient = &http.Client{Transport: srv.Client().Transport, CheckRedirect: prev.CheckRedirect}
	t.Cleanup(func() { credentialedClient = prev })
	err := requestError(t, srv.URL)
	var te *TransportError
	if want := "GET " + srv.URL + "/v1/config/self?profile=p: The read operation timed out"; !errors.As(err, &te) || te.Message != want {
		t.Fatalf("stalled tls body: %v", err)
	}
	if !strings.HasPrefix(srv.URL, "https://127.0.0.1:") {
		t.Fatalf("fixture url %s", srv.URL)
	}
}
