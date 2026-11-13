package nodehttp

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/desktop/hardening"
	whatwg "github.com/nlnwa/whatwg-url/url"
)

// Wait of one connection attempt before Node moves to the next resolved address
const autoSelectFamilyAttemptTimeout = 250 * time.Millisecond

// Error of a Node http request with the code and message Node gives it
type Error struct {
	Code    string
	Message string
	// Cause, every attempt error joined for an AggregateError
	Err error
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Err }

// Parses raw as new URL(raw), the TypeError Invalid URL when it does not parse
func ParseURL(raw string) (*whatwg.Url, error) {
	u, err := whatwg.Parse(raw)
	if err != nil {
		return nil, &Error{Code: "ERR_INVALID_URL", Message: "Invalid URL", Err: err}
	}
	return u, nil
}

// Decodes base64 as Buffer.from does, either alphabet, stray characters skipped, stopping at padding
func DecodeBase64(s string) []byte {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '-':
			b.WriteByte('+')
		case c == '_':
			b.WriteByte('/')
		case c == '=':
			i = len(s)
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/':
			b.WriteByte(c)
		}
	}
	clean := b.String()
	if len(clean)%4 == 1 {
		clean = clean[:len(clean)-1]
	}
	out, _ := base64.RawStdEncoding.DecodeString(clean)
	return out
}

// Decodes s as decodeURIComponent, URIError URI malformed on a bad escape or byte sequence
func DecodeURIComponent(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
			return "", errURIMalformed
		}
		b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
		i += 2
	}
	out := b.String()
	if !utf8.ValidString(out) {
		return "", errURIMalformed
	}
	return out, nil
}

var errURIMalformed = &Error{Message: "URI malformed"}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}

// Connection pool of a Node http.Agent
type Agent struct{ t *http.Transport }

// Keep alive agent with at most maxSockets sockets per origin and no idle socket timeout
func NewAgent(maxSockets int) *Agent {
	return &Agent{t: &http.Transport{DialContext: dial, DialTLSContext: dialTLS, DisableCompression: true, MaxConnsPerHost: maxSockets}}
}

// Agent of http.get and https.get, keep alive sockets freed after 5 seconds idle
var globalAgent = &Agent{t: &http.Transport{DialContext: dial, DialTLSContext: dialTLS, DisableCompression: true, IdleConnTimeout: 5 * time.Second}}

// Sends a GET as Node http.get or https.get does, module being the protocol of the client the caller picked
func Get(ctx context.Context, u *whatwg.Url, module string, header http.Header) (*http.Response, error) {
	return globalAgent.Get(ctx, u, module, header)
}

// Sends a GET as Node http.request does over this agent, explicit headers winning over URL credentials
func (a *Agent) Get(ctx context.Context, u *whatwg.Url, module string, header http.Header) (*http.Response, error) {
	h := header.Clone()
	if h == nil {
		h = http.Header{}
	}
	if (u.Username() != "" || u.Password() != "") && h.Get("Authorization") == "" {
		user, err := DecodeURIComponent(u.Username())
		if err != nil {
			return nil, err
		}
		pass, err := DecodeURIComponent(u.Password())
		if err != nil {
			return nil, err
		}
		h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	}
	if u.Protocol() != module {
		return nil, &Error{Code: "ERR_INVALID_PROTOCOL", Message: `Protocol "` + u.Protocol() + `" not supported. Expected "` + module + `"`}
	}
	target := &url.URL{Scheme: strings.TrimSuffix(module, ":"), Host: u.Host()}
	if p := u.Pathname(); strings.HasPrefix(p, "//") {
		target.Path, _ = url.PathUnescape(p)
		target.RawPath = p
	} else {
		target.Opaque = p
	}
	if q := u.Search(); q != "" {
		target.RawQuery = q[1:]
	}
	req := (&http.Request{Method: http.MethodGet, URL: target, Host: u.Host(), Header: h, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}).WithContext(ctx)
	if _, ok := h["User-Agent"]; !ok {
		h.Set("User-Agent", "")
	}
	if h.Get("Connection") == "" {
		h.Set("Connection", "keep-alive")
	}
	res, err := a.t.RoundTrip(req)
	if err != nil {
		return nil, requestError(err)
	}
	res.Body = &body{rc: res.Body}
	return res, nil
}

// Response body whose read failures carry the Node texts of an interrupted response
type body struct{ rc io.ReadCloser }

func (b *body) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if err != nil && err != io.EOF {
		err = bodyError(err)
	}
	return n, err
}

func (b *body) Close() error { return b.rc.Close() }

// Node error of a failed response body read
func bodyError(err error) error {
	var ne *Error
	if errors.As(err, &ne) {
		return ne
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return &Error{Code: "ECONNRESET", Message: "aborted", Err: err}
	}
	if code := errnoCode(err); code != "" {
		return &Error{Code: code, Message: "read " + code, Err: err}
	}
	return &Error{Code: "ECONNRESET", Message: "aborted", Err: err}
}

// Node error of a request that failed before its response arrived
func requestError(err error) error {
	var ne *Error
	if errors.As(err, &ne) {
		return ne
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if code := errnoCode(err); code != "" {
		return &Error{Code: code, Message: "read " + code, Err: err}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "server closed idle connection") {
		return &Error{Code: "ECONNRESET", Message: "socket hang up", Err: err}
	}
	if strings.Contains(err.Error(), "malformed HTTP") {
		return &Error{Code: "HPE_INVALID_CONSTANT", Message: "Parse Error: Expected HTTP/", Err: err}
	}
	return err
}

// Node code of the errno an error carries, empty without one
func errnoCode(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return ""
	}
	if code, ok := socketCodes[errno]; ok {
		return code
	}
	code := hardening.NodeCode(errno)
	if code == "UNKNOWN" {
		return ""
	}
	return code
}

// Connects as net.connect with lookup of every address and autoSelectFamily
func dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		found, lerr := net.DefaultResolver.LookupIPAddr(ctx, host)
		if lerr != nil {
			return nil, lookupError(lerr, host)
		}
		for _, a := range found {
			ips = append(ips, a.IP)
		}
		if len(ips) == 0 {
			return nil, &Error{Code: "ENOTFOUND", Message: "getaddrinfo ENOTFOUND " + host}
		}
	}
	order := interleaveFamilies(ips)
	var d net.Dialer
	if len(order) == 1 {
		c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(order[0].String(), port))
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, connectError(err, order[0], port)
		}
		return c, nil
	}
	var attempts []error
	for i, ip := range order {
		actx, cancel := ctx, context.CancelFunc(func() {})
		last := i == len(order)-1
		if !last {
			actx, cancel = context.WithTimeout(ctx, autoSelectFamilyAttemptTimeout)
		}
		c, err := d.DialContext(actx, "tcp", net.JoinHostPort(ip.String(), port))
		timedOut := !last && actx.Err() == context.DeadlineExceeded && ctx.Err() == nil
		cancel()
		if err == nil {
			return c, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if timedOut {
			attempts = append(attempts, &Error{Code: "ETIMEDOUT", Message: "connect ETIMEDOUT " + hostPort(ip, port)})
			continue
		}
		attempts = append(attempts, connectError(err, ip, port))
	}
	return nil, &Error{Code: attempts[0].(*Error).Code, Message: "", Err: errors.Join(attempts...)}
}

// Addresses alternating families, starting with the family of the first one
func interleaveFamilies(ips []net.IP) []net.IP {
	var first, second []net.IP
	firstIs4 := ips[0].To4() != nil
	for _, ip := range ips {
		if (ip.To4() != nil) == firstIs4 {
			first = append(first, ip)
		} else {
			second = append(second, ip)
		}
	}
	var out []net.IP
	for i := 0; i < len(first) || i < len(second); i++ {
		if i < len(first) {
			out = append(out, first[i])
		}
		if i < len(second) {
			out = append(out, second[i])
		}
	}
	return out
}

// Address as Node prints it in connect errors, IPv6 without brackets
func hostPort(ip net.IP, port string) string { return ip.String() + ":" + port }

// Node error of a failed name lookup
func lookupError(err error, host string) error {
	code := "EAI_FAIL"
	var dns *net.DNSError
	if errors.As(err, &dns) {
		switch {
		case dns.IsNotFound:
			code = "ENOTFOUND"
		case dns.IsTimeout, dns.IsTemporary:
			code = "EAI_AGAIN"
		}
	}
	return &Error{Code: code, Message: "getaddrinfo " + code + " " + host, Err: err}
}

// Node error of a failed TCP connect to ip and port
func connectError(err error, ip net.IP, port string) *Error {
	code := errnoCode(err)
	if code == "" {
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() {
			code = "ETIMEDOUT"
		} else {
			code = hardening.NodeCode(err)
		}
	}
	return &Error{Code: code, Message: "connect " + code + " " + hostPort(ip, port), Err: err}
}

// Connects and runs the TLS handshake as tls.connect does, checks in the OpenSSL and Node order
func dialTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	raw, err := dial(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{InsecureSkipVerify: true, VerifyConnection: verifyConnection(host)}
	if net.ParseIP(host) == nil {
		cfg.ServerName = host
	}
	conn := tls.Client(raw, cfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		raw.Close()
		var ne *Error
		switch {
		case errors.As(err, &ne):
			return nil, ne
		case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errnoCode(err) == "ECONNRESET":
			return nil, &Error{Code: "ECONNRESET", Message: "Client network socket disconnected before secure TLS connection was established", Err: err}
		}
		return nil, err
	}
	return conn, nil
}
