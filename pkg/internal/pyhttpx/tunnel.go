package pyhttpx

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// Network dial a tunnel or a direct connection runs over
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Forward proxy for an HTTP target and none for HTTPS, whose tunnel the wire TLS dial opens
func PlainProxy(proxyURL *url.URL) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		if req.URL == nil || req.URL.Scheme != "http" {
			return nil, nil
		}
		return ForwardProxy(proxyURL)
	}
}

// Forward proxy of a plain HTTP request, an HTTPS proxy given as http for the wire dial TLS
func ForwardProxy(proxyURL *url.URL) (*url.URL, error) {
	if proxyURL == nil {
		return nil, nil
	}
	switch strings.ToLower(proxyURL.Scheme) {
	case "http":
		return proxyURL, nil
	case "https":
		forward := *proxyURL
		forward.Scheme, forward.Host = "http", proxyHostPort(proxyURL)
		return &forward, nil
	case "socks5", "socks5h":
		return nil, nil
	}
	return nil, unknownProxyScheme(proxyURL)
}

// ValueError httpx Proxy raises for a scheme it has no transport for
func unknownProxyScheme(proxyURL *url.URL) error {
	return fmt.Errorf("Unknown scheme for proxy URL URL('%s')", proxyURL.String())
}

// TLS connection to address under a wire layer, tunnelled through the proxy when one is given, HTTP/1.1 offered by ALPN
func DialWireTLS(ctx context.Context, dial DialFunc, proxyURL *url.URL, network, address string, cfg *tls.Config) (net.Conn, error) {
	raw, err := dialTarget(ctx, dial, proxyURL, network, address)
	if err != nil {
		return nil, err
	}
	conf := cfg.Clone()
	if conf == nil {
		conf = &tls.Config{}
	}
	if conf.ServerName == "" {
		host, _, _ := net.SplitHostPort(address)
		conf.ServerName = host
	}
	conf.NextProtos = []string{"http/1.1"}
	conn := tls.Client(raw, conf)
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return NewWireConn(conn), nil
}

// Plaintext connection under a wire layer, connected through a SOCKS proxy or over TLS to an HTTPS forward proxy itself
func DialWire(ctx context.Context, dial DialFunc, proxyURL *url.URL, network, address string) (net.Conn, error) {
	var conn net.Conn
	var err error
	scheme := ""
	if proxyURL != nil {
		scheme = strings.ToLower(proxyURL.Scheme)
	}
	switch scheme {
	case "socks5", "socks5h":
		conn, err = dialTarget(ctx, dial, proxyURL, network, address)
	case "https":
		conn, err = dialProxyTLS(ctx, dial, proxyURL)
	case "", "http":
		conn, err = dial(ctx, network, address)
	default:
		err = unknownProxyScheme(proxyURL)
	}
	if err != nil {
		return nil, err
	}
	return NewWireConn(conn), nil
}

// TLS connection to an HTTPS proxy offering HTTP/1.1, the raw connection closed when the handshake fails
func dialProxyTLS(ctx context.Context, dial DialFunc, proxyURL *url.URL) (net.Conn, error) {
	conn, err := dial(ctx, "tcp", proxyHostPort(proxyURL))
	if err != nil {
		return nil, err
	}
	tlsConn := tls.Client(conn, &tls.Config{ServerName: proxyURL.Hostname(), NextProtos: []string{"http/1.1"}})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return tlsConn, nil
}

// Byte stream to the target, a CONNECT tunnel through an HTTP or HTTPS proxy or a SOCKS5 connect
func dialTarget(ctx context.Context, dial DialFunc, proxyURL *url.URL, network, address string) (net.Conn, error) {
	if proxyURL == nil {
		return dial(ctx, network, address)
	}
	switch strings.ToLower(proxyURL.Scheme) {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if proxyURL.User != nil {
			pw, _ := proxyURL.User.Password()
			auth = &proxy.Auth{User: proxyURL.User.Username(), Password: pw}
		}
		d, err := proxy.SOCKS5("tcp", proxyHostPort(proxyURL), auth, contextDialer(dial))
		if err != nil {
			return nil, err
		}
		return d.(proxy.ContextDialer).DialContext(ctx, network, address)
	case "http", "https":
		return connectTunnel(ctx, dial, proxyURL, address)
	}
	return nil, unknownProxyScheme(proxyURL)
}

// Proxy address with the default port of its scheme
func proxyHostPort(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	port := map[string]string{"https": "443", "socks5": "1080", "socks5h": "1080"}[strings.ToLower(u.Scheme)]
	if port == "" {
		port = "80"
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// Dialer view of a dial function for the SOCKS client
type contextDialer DialFunc

func (d contextDialer) Dial(network, address string) (net.Conn, error) {
	return d(context.Background(), network, address)
}

func (d contextDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d(ctx, network, address)
}

// CONNECT tunnel with the Host, Accept and Proxy-Authorization lines httpcore sends
func connectTunnel(ctx context.Context, dial DialFunc, proxyURL *url.URL, address string) (net.Conn, error) {
	var conn net.Conn
	var err error
	if strings.EqualFold(proxyURL.Scheme, "https") {
		conn, err = dialProxyTLS(ctx, dial, proxyURL)
	} else {
		conn, err = dial(ctx, "tcp", proxyHostPort(proxyURL))
	}
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nAccept: */*\r\n", address, address)
	if proxyURL.User != nil {
		pw, _ := proxyURL.User.Password()
		fmt.Fprintf(&b, "Proxy-Authorization: Basic %s\r\n", base64.StdEncoding.EncodeToString([]byte(proxyURL.User.Username()+":"+pw)))
	}
	b.WriteString("\r\n")
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	if _, err := conn.Write([]byte(b.String())); err != nil {
		stopCancel()
		_ = conn.Close()
		return nil, tunnelExchangeError(ctx, err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if !stopCancel() && err == nil {
		_ = resp.Body.Close()
		err = ctx.Err()
	}
	if err != nil {
		_ = conn.Close()
		return nil, tunnelExchangeError(ctx, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_ = conn.Close()
		return nil, errors.New(resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

// Cancellation of the context when it cut the CONNECT exchange short, the exchange error otherwise
func tunnelExchangeError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	}
	return err
}

// Tunnel connection replaying bytes the CONNECT reply read past its end
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// Connection under the buffered reader
func (c *bufferedConn) NetConn() net.Conn { return c.Conn }
