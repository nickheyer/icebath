package browser

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// WebSocket opcodes from RFC 6455 section 5.2
const (
	opContinuation byte = 0x0
	opText         byte = 0x1
	opBinary       byte = 0x2
	opClose        byte = 0x8
	opPing         byte = 0x9
	opPong         byte = 0xA
)

// Close status codes used by the client
const (
	CloseNormal          = 1000
	CloseGoingAway       = 1001
	CloseProtocolError   = 1002
	CloseUnsupportedData = 1003
	CloseMessageTooBig   = 1009
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Largest payload one control frame may carry
const maxControlPayload = 125

// ErrWSClosed reports the peer closed the websocket with its status code and reason
type ErrWSClosed struct {
	Code   int
	Reason string
}

func (e *ErrWSClosed) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("websocket closed (%d)", e.Code)
	}
	return fmt.Sprintf("websocket closed (%d): %s", e.Code, e.Reason)
}

// WSOptions tunes a websocket dial
type WSOptions struct {
	// Zero means no limit on an assembled message
	MaxMessageSize int64
	// Zero means a 10 second handshake deadline
	HandshakeTimeout time.Duration
	Header           http.Header
	TLS              *tls.Config
}

// WSConn is one RFC 6455 client connection with reassembly, ping replies, and close handshake
type WSConn struct {
	conn    net.Conn
	br      *bufio.Reader
	writeMu sync.Mutex
	maxSize int64
	closed  bool
	closeMu sync.Mutex
	// Fragments of the message being reassembled
	fragOp   byte
	fragBuf  []byte
	fragging bool
	url      string
}

// DialWS performs the opening handshake against a ws or wss URL, tunnelling through an environment proxy for non loopback hosts
func DialWS(ctx context.Context, rawURL string, opts WSOptions) (*WSConn, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("websocket url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "ws" && scheme != "wss" {
		return nil, fmt.Errorf("websocket url must be ws:// or wss://, got %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, errors.New("websocket url has no host")
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if scheme == "wss" {
			port = "443"
		}
	}
	timeout := opts.HandshakeTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addr := net.JoinHostPort(host, port)
	netConn, err := dialMaybeProxied(dialCtx, scheme, host, addr)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	_ = netConn.SetDeadline(deadline)
	if scheme == "wss" {
		cfg := opts.TLS
		if cfg == nil {
			cfg = &tls.Config{}
		} else {
			cfg = cfg.Clone()
		}
		if cfg.ServerName == "" {
			cfg.ServerName = host
		}
		tlsConn := tls.Client(netConn, cfg)
		if err := tlsConn.HandshakeContext(dialCtx); err != nil {
			netConn.Close()
			return nil, fmt.Errorf("tls handshake: %w", err)
		}
		netConn = tlsConn
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		netConn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	var req strings.Builder
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\n", path)
	hostHeader := host
	if (scheme == "ws" && port != "80") || (scheme == "wss" && port != "443") {
		hostHeader = addr
	}
	fmt.Fprintf(&req, "Host: %s\r\n", hostHeader)
	req.WriteString("Upgrade: websocket\r\nConnection: Upgrade\r\n")
	fmt.Fprintf(&req, "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", key)
	for name, values := range opts.Header {
		for _, v := range values {
			fmt.Fprintf(&req, "%s: %s\r\n", name, v)
		}
	}
	req.WriteString("\r\n")
	if _, err := io.WriteString(netConn, req.String()); err != nil {
		netConn.Close()
		return nil, fmt.Errorf("websocket handshake write: %w", err)
	}
	br := bufio.NewReaderSize(netConn, 64*1024)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		netConn.Close()
		return nil, fmt.Errorf("websocket handshake read: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		netConn.Close()
		return nil, fmt.Errorf("websocket handshake rejected: %s %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") || !headerHasToken(resp.Header, "Connection", "upgrade") {
		netConn.Close()
		return nil, errors.New("websocket handshake missing upgrade headers")
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != wsAcceptKey(key) {
		netConn.Close()
		return nil, errors.New("websocket handshake returned a bad Sec-WebSocket-Accept")
	}
	_ = netConn.SetDeadline(time.Time{})
	return &WSConn{conn: netConn, br: br, maxSize: opts.MaxMessageSize, url: rawURL}, nil
}

// Dials the host directly for loopback targets and through the environment HTTP proxy otherwise
func dialMaybeProxied(ctx context.Context, scheme, host, addr string) (net.Conn, error) {
	dialer := &net.Dialer{}
	if isLoopbackHost(host) {
		return dialer.DialContext(ctx, "tcp", addr)
	}
	proxyScheme := "http"
	if scheme == "wss" {
		proxyScheme = "https"
	}
	proxyURL, err := http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: proxyScheme, Host: addr}})
	if err != nil || proxyURL == nil {
		return dialer.DialContext(ctx, "tcp", addr)
	}
	proxyPort := proxyURL.Port()
	if proxyPort == "" {
		proxyPort = "80"
		if proxyURL.Scheme == "https" {
			proxyPort = "443"
		}
	}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(proxyURL.Hostname(), proxyPort))
	if err != nil {
		return nil, fmt.Errorf("proxy dial: %w", err)
	}
	if proxyURL.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: proxyURL.Hostname()})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("proxy tls: %w", err)
		}
		conn = tlsConn
	}
	var req strings.Builder
	fmt.Fprintf(&req, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
	if proxyURL.User != nil {
		pw, _ := proxyURL.User.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(proxyURL.User.Username() + ":" + pw))
		fmt.Fprintf(&req, "Proxy-Authorization: Basic %s\r\n", cred)
	}
	req.WriteString("\r\n")
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := io.WriteString(conn, req.String()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("proxy connect: %w", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("proxy connect: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("proxy connect refused: %s", resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// Reports whether a header carries the token in a comma separated list, case insensitively
func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// Sec-WebSocket-Accept value for a client key
func wsAcceptKey(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// Reports whether a host string names a loopback address
func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// URL returns the endpoint this connection was dialed with
func (c *WSConn) URL() string { return c.url }

// WriteText sends one unfragmented masked text frame
func (c *WSConn) WriteText(ctx context.Context, payload []byte) error {
	return c.writeFrame(ctx, opText, payload)
}

// WriteBinary sends one unfragmented masked binary frame
func (c *WSConn) WriteBinary(ctx context.Context, payload []byte) error {
	return c.writeFrame(ctx, opBinary, payload)
}

// Ping sends a ping control frame with an optional payload
func (c *WSConn) Ping(ctx context.Context, payload []byte) error {
	if len(payload) > maxControlPayload {
		return errors.New("ping payload exceeds 125 bytes")
	}
	return c.writeFrame(ctx, opPing, payload)
}

// Writes one complete masked frame under the write lock
func (c *WSConn) writeFrame(ctx context.Context, op byte, payload []byte) error {
	frame, err := buildClientFrame(op, payload)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetWriteDeadline(deadline)
	} else {
		_ = c.conn.SetWriteDeadline(time.Time{})
	}
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetWriteDeadline(time.Now()) })
	defer stop()
	_, err = c.conn.Write(frame)
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// Encodes a FIN frame with a fresh mask key as a client must send it
func buildClientFrame(op byte, payload []byte) ([]byte, error) {
	if op&0x8 != 0 && len(payload) > maxControlPayload {
		return nil, errors.New("control frame payload exceeds 125 bytes")
	}
	n := len(payload)
	head := make([]byte, 2, 14)
	head[0] = 0x80 | (op & 0x0F)
	switch {
	case n <= 125:
		head[1] = 0x80 | byte(n)
	case n <= 0xFFFF:
		head[1] = 0x80 | 126
		head = binary.BigEndian.AppendUint16(head, uint16(n))
	default:
		head[1] = 0x80 | 127
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return nil, err
	}
	frame := make([]byte, 0, len(head)+4+n)
	frame = append(frame, head...)
	frame = append(frame, mask...)
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	return append(frame, masked...), nil
}

// ReadMessage returns the next complete text or binary message, answering pings and completing a close handshake meanwhile
func (c *WSConn) ReadMessage(ctx context.Context) (byte, []byte, error) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetReadDeadline(deadline)
	} else {
		_ = c.conn.SetReadDeadline(time.Time{})
	}
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetReadDeadline(time.Now()) })
	defer stop()
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			if ctx.Err() != nil {
				return 0, nil, ctx.Err()
			}
			return 0, nil, err
		}
		switch op {
		case opPing:
			if err := c.writeFrame(context.Background(), opPong, payload); err != nil {
				return 0, nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			code, reason := CloseNormal, ""
			if len(payload) >= 2 {
				code = int(binary.BigEndian.Uint16(payload[:2]))
				reason = string(payload[2:])
			}
			if !c.markClosed() {
				_ = c.writeFrame(context.Background(), opClose, closePayload(code, ""))
			}
			c.conn.Close()
			return 0, nil, &ErrWSClosed{Code: code, Reason: reason}
		case opText, opBinary:
			if c.fragging {
				c.failProtocol()
				return 0, nil, errors.New("websocket: new data frame while a fragmented message is open")
			}
			if fin {
				return op, payload, nil
			}
			c.fragging, c.fragOp, c.fragBuf = true, op, append([]byte(nil), payload...)
		case opContinuation:
			if !c.fragging {
				c.failProtocol()
				return 0, nil, errors.New("websocket: continuation frame without a started message")
			}
			c.fragBuf = append(c.fragBuf, payload...)
			if c.maxSize > 0 && int64(len(c.fragBuf)) > c.maxSize {
				_ = c.SendClose(CloseMessageTooBig, "message too big")
				c.conn.Close()
				return 0, nil, errors.New("websocket: message exceeds the size limit")
			}
			if fin {
				op, buf := c.fragOp, c.fragBuf
				c.fragging, c.fragBuf = false, nil
				return op, buf, nil
			}
		default:
			c.failProtocol()
			return 0, nil, fmt.Errorf("websocket: unknown opcode %#x", op)
		}
	}
}

// Sends a protocol error close and drops the connection
func (c *WSConn) failProtocol() {
	if !c.markClosed() {
		_ = c.writeFrame(context.Background(), opClose, closePayload(CloseProtocolError, "protocol error"))
	}
	c.conn.Close()
}

// Reads one frame header and payload, rejecting reserved bits, masked server frames, and oversized control frames
func (c *WSConn) readFrame() (bool, byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		return false, 0, nil, err
	}
	fin := head[0]&0x80 != 0
	if head[0]&0x70 != 0 {
		c.failProtocol()
		return false, 0, nil, errors.New("websocket: reserved bits set")
	}
	op := head[0] & 0x0F
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
		if length&(1<<63) != 0 {
			c.failProtocol()
			return false, 0, nil, errors.New("websocket: invalid 64 bit length")
		}
	}
	if op&0x8 != 0 && (length > maxControlPayload || !fin) {
		c.failProtocol()
		return false, 0, nil, errors.New("websocket: malformed control frame")
	}
	if masked {
		c.failProtocol()
		return false, 0, nil, errors.New("websocket: server sent a masked frame")
	}
	if c.maxSize > 0 && int64(length) > c.maxSize {
		_ = c.SendClose(CloseMessageTooBig, "message too big")
		c.conn.Close()
		return false, 0, nil, errors.New("websocket: frame exceeds the size limit")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	return fin, op, payload, nil
}

// Encodes a close frame body from a status code and reason
func closePayload(code int, reason string) []byte {
	buf := make([]byte, 2, 2+len(reason))
	binary.BigEndian.PutUint16(buf, uint16(code))
	return append(buf, reason...)
}

// Flags the close handshake as started and reports whether it already was
func (c *WSConn) markClosed() bool {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	already := c.closed
	c.closed = true
	return already
}

// SendClose writes a close frame and leaves the peer's reply to whoever reads the connection
func (c *WSConn) SendClose(code int, reason string) error {
	if c.markClosed() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return c.writeFrame(ctx, opClose, closePayload(code, reason))
}

// ForceClose drops the socket without a close handshake
func (c *WSConn) ForceClose() {
	c.markClosed()
	c.conn.Close()
}

// Close sends a close frame, waits briefly for the peer's close, and shuts the socket
func (c *WSConn) Close(code int, reason string) error {
	if c.markClosed() {
		c.conn.Close()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.writeFrame(ctx, opClose, closePayload(code, reason))
	_ = c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, op, _, err := c.readFrame()
		if err != nil || op == opClose {
			break
		}
	}
	c.conn.Close()
	return nil
}
