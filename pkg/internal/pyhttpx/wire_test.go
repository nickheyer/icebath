package pyhttpx

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Server that records each raw request head byte for byte before replying
func newRawServer(t *testing.T, reply string) (string, chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	heads := make(chan string, 16)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				br := bufio.NewReader(conn)
				for {
					var head strings.Builder
					length := 0
					for {
						line, err := br.ReadString('\n')
						if err != nil {
							return
						}
						head.WriteString(line)
						if line == "\r\n" {
							break
						}
						if name, value, ok := strings.Cut(strings.TrimRight(line, "\r\n"), ": "); ok && strings.EqualFold(name, "Content-Length") {
							for _, r := range value {
								length = length*10 + int(r-'0')
							}
						}
					}
					if _, err := io.CopyN(io.Discard, br, int64(length)); err != nil {
						return
					}
					heads <- head.String()
					if _, err := io.WriteString(conn, reply); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), heads
}

// Client whose connections carry the wire layer, the way the provider transports dial
func wireClient(decoders []string) *http.Client {
	dial := (&net.Dialer{}).DialContext
	return &http.Client{Transport: WireTransport{Base: &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return DialWire(ctx, dial, nil, network, address)
		},
	}, Decoders: decoders}}
}

func TestWireRequestWritesItsLinesInOrderAndSpelling(t *testing.T) {
	addr, heads := newRawServer(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	client := wireClient([]string{"gzip", "deflate"})
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/chat/completions", strings.NewReader(`{"a":1}`))
		h := NewHeaders(HeaderLine{"Accept-Encoding", "gzip, deflate"}, HeaderLine{"Connection", "keep-alive"},
			HeaderLine{"Authorization", "Bearer k"}, HeaderLine{"Accept", "application/json"}, HeaderLine{"Content-Type", "application/json"},
			HeaderLine{"User-Agent", "OpenAI/Python 2.54.0"}, HeaderLine{"X-Stainless-Lang", "python"}, HeaderLine{"x-stainless-retry-count", "0"})
		resp, err := client.Do(WithWire(req, h))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		want := "POST /v1/chat/completions HTTP/1.1\r\nHost: " + addr + "\r\nAccept-Encoding: gzip, deflate\r\nConnection: keep-alive\r\n" +
			"Authorization: Bearer k\r\nAccept: application/json\r\nContent-Type: application/json\r\nUser-Agent: OpenAI/Python 2.54.0\r\n" +
			"X-Stainless-Lang: python\r\nx-stainless-retry-count: 0\r\nContent-Length: 7\r\n\r\n"
		if got := <-heads; got != want {
			t.Fatalf("request %d head\n%q\nwant\n%q", i, got, want)
		}
	}
}

func TestPlainRequestOnAWireConnectionKeepsTheTransportHead(t *testing.T) {
	addr, heads := newRawServer(t, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
	client := wireClient(nil)
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/models", nil)
	req.Header.Set("X-B", "2")
	req.Header.Set("X-A", "1")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := <-heads; !strings.Contains(got, "X-A: 1\r\nX-B: 2\r\n") {
		t.Fatalf("plain head %q", got)
	}
}

func TestHeadersEditLikeHttpx(t *testing.T) {
	h := NewHeaders(HeaderLine{"Accept", "*/*"}, HeaderLine{"Accept-Encoding", "gzip, deflate"}, HeaderLine{"Connection", "keep-alive"},
		HeaderLine{"User-Agent", "python-httpx/0.28.1"})
	h.Update([]HeaderLine{{"Accept", "application/json"}, {"User-Agent", "OpenAI/Python 2.54.0"}})
	h.Set("x-stainless-retry-count", "0")
	h.Set("accept", "text/plain")
	var got []string
	for _, l := range h.Lines() {
		got = append(got, l.Name+": "+l.Value)
	}
	want := "Accept-Encoding: gzip, deflate|Connection: keep-alive|accept: text/plain|User-Agent: OpenAI/Python 2.54.0|x-stainless-retry-count: 0"
	if strings.Join(got, "|") != want {
		t.Fatalf("lines %q", strings.Join(got, "|"))
	}
}

func encoded(t *testing.T, coding string, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	var w io.WriteCloser
	switch coding {
	case "gzip":
		w = gzip.NewWriter(&b)
	case "zlib":
		w = zlib.NewWriter(&b)
	case "raw":
		w, _ = flate.NewWriter(&b, flate.DefaultCompression)
	}
	_, _ = w.Write(data)
	_ = w.Close()
	return b.Bytes()
}

func TestWireResponseDecodesTheAdvertisedCodingsOnly(t *testing.T) {
	payload := []byte(`{"ok": true}`)
	cases := []struct {
		header string
		body   []byte
		want   []byte
	}{
		{"gzip", encoded(t, "gzip", payload), payload},
		{"deflate", encoded(t, "zlib", payload), payload},
		{"deflate", encoded(t, "raw", payload), payload},
		{"deflate, gzip", encoded(t, "gzip", encoded(t, "zlib", payload)), payload},
		{"br", []byte("not decoded"), []byte("not decoded")},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Encoding", tc.header)
			_, _ = w.Write(tc.body)
		}))
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		resp, err := wireClient([]string{"gzip", "deflate"}).Do(WithWire(req, NewHeaders(HeaderLine{"Accept-Encoding", "gzip, deflate"})))
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		srv.Close()
		if err != nil || !bytes.Equal(got, tc.want) || resp.Header.Get("Content-Encoding") != tc.header {
			t.Fatalf("%s decoded %q err %v header %q", tc.header, got, err, resp.Header.Get("Content-Encoding"))
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write([]byte("plain text, not gzip"))
	}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := wireClient([]string{"gzip", "deflate"}).Do(WithWire(req, NewHeaders(HeaderLine{"Accept-Encoding", "gzip, deflate"})))
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var de *DecodingError
	if !errors.As(err, &de) {
		t.Fatalf("a corrupt gzip body read %v", err)
	}
}

func TestWireTLSDialTunnelsThroughAConnectProxy(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Proto+" "+r.Header.Get("User-Agent"))
	}))
	defer target.Close()
	connects := make(chan string, 4)
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyLn.Close()
	go func() {
		for {
			conn, err := proxyLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				br := bufio.NewReader(conn)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				connects <- req.Method + " " + req.RequestURI + " " + req.Header.Get("Proxy-Authorization") + " " + req.Header.Get("Accept")
				up, err := net.Dial("tcp", req.RequestURI)
				if err != nil {
					return
				}
				defer up.Close()
				_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
				go func() { _, _ = io.Copy(up, br) }()
				_, _ = io.Copy(conn, up)
			}(conn)
		}
	}()
	proxyURL, _ := url.Parse("http://user:pw@" + proxyLn.Addr().String())
	dial := (&net.Dialer{}).DialContext
	cfg := &tls.Config{RootCAs: target.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
	client := &http.Client{Transport: WireTransport{Base: &http.Transport{
		Proxy: PlainProxy(proxyURL),
		DialTLSContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return DialWireTLS(ctx, dial, proxyURL, network, address, cfg)
		},
	}}}
	req, _ := http.NewRequest(http.MethodGet, target.URL, nil)
	resp, err := client.Do(WithWire(req, NewHeaders(HeaderLine{"User-Agent", "OpenAI/Python 2.54.0"})))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "HTTP/1.1 OpenAI/Python 2.54.0" {
		t.Fatalf("tunnelled reply %q", body)
	}
	if got := <-connects; got != "CONNECT "+strings.TrimPrefix(target.URL, "https://")+" Basic dXNlcjpwdw== */*" {
		t.Fatalf("connect %q", got)
	}
}
