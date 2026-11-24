package mailutil

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/netpref"
)

// Function that opens a network connection, the shape of net.Dialer.DialContext
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

var preferredDial = netpref.DialContext((&net.Dialer{}).DialContext)

// Connection over whichever address family answers, IPv4 tried first while the process prefers it
func Dial(ctx context.Context, network, address string) (net.Conn, error) {
	return preferredDial(ctx, network, address)
}

// Connection over IPv4 only, each IPv4 address of the host tried in turn
func DialIPv4(ctx context.Context, network, address string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp6":
		network = "tcp4"
	case "udp", "udp6":
		network = "udp4"
	}
	var d net.Dialer
	return d.DialContext(ctx, network, address)
}

// Network connection whose every read and write must make progress within the timeout, no limit when it is zero
type timedConn struct {
	net.Conn
	timeout time.Duration
}

// Reads with the read deadline moved to the timeout from now
func (c *timedConn) Read(p []byte) (int, error) {
	if c.timeout > 0 {
		if err := c.Conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
			return 0, err
		}
	}
	return c.Conn.Read(p)
}

// Writes with the write deadline moved to the timeout from now
func (c *timedConn) Write(p []byte) (int, error) {
	if c.timeout > 0 {
		if err := c.Conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
			return 0, err
		}
	}
	return c.Conn.Write(p)
}

const readBufferSize = 32 << 10

// Connection state the IMAP and SMTP clients share, one command running at a time
type session struct {
	mu        sync.Mutex
	proto     string
	host      string
	timeout   time.Duration
	raw       net.Conn
	conn      net.Conn
	r         *bufio.Reader
	w         *bufio.Writer
	secure    bool
	plainAuth bool
	broken    error
	closed    atomic.Bool
}

// Lets Login send the user name and password over a connection without TLS, which it refuses otherwise unless the server is on this machine
func (s *session) AllowPlaintextLogin() {
	s.mu.Lock()
	s.plainAuth = true
	s.mu.Unlock()
}

// Error for credentials about to cross a connection that is neither encrypted nor to this machine, unless the caller allowed that
func (s *session) checkCredentials() error {
	if s.secure || s.plainAuth || isLoopback(s.host) {
		return nil
	}
	return fmt.Errorf("%s: %w: refusing to send credentials to %s without TLS", s.proto, ErrInsecure, s.host)
}

// Whether a host names this machine: localhost or a loopback address
func isLoopback(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// Dials the server, running the TLS handshake straight away when a configuration is given
func (s *session) open(ctx context.Context, proto string, dial DialFunc, host string, port int, timeout time.Duration, config *tls.Config) error {
	if dial == nil {
		dial = Dial
	}
	if host == "" {
		host = "localhost"
	}
	s.proto, s.host, s.timeout = proto, host, timeout
	dctx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		dctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	raw, err := dial(dctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("%s: %w", proto, err)
	}
	s.raw = raw
	s.conn = &timedConn{Conn: raw, timeout: timeout}
	if config == nil {
		s.buffer()
		return nil
	}
	if err := s.handshake(ctx, config); err != nil {
		_ = raw.Close()
		return err
	}
	return nil
}

// Runs the TLS handshake over the connection within the timeout, the certificate checked against the host unless the configuration names another server
func (s *session) handshake(ctx context.Context, config *tls.Config) error {
	if config == nil {
		config = &tls.Config{}
	} else {
		config = config.Clone()
	}
	if config.ServerName == "" {
		config.ServerName = s.host
	}
	hctx := ctx
	if s.timeout > 0 {
		var cancel context.CancelFunc
		hctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}
	secured := tls.Client(s.conn, config)
	if err := secured.HandshakeContext(hctx); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%s: TLS handshake with %s: %w", s.proto, config.ServerName, ctxErr)
		}
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
			return fmt.Errorf("%s: TLS handshake with %s: %w", s.proto, config.ServerName, err)
		}
		return fmt.Errorf("%s: %w", s.proto, &TLSError{Host: config.ServerName, Err: err})
	}
	s.conn, s.secure = secured, true
	s.buffer()
	return nil
}

// Fresh reader and writer over the current connection, whatever was buffered from the one before dropped
func (s *session) buffer() {
	s.r = bufio.NewReaderSize(s.conn, readBufferSize)
	s.w = bufio.NewWriter(s.conn)
}

// Runs fn as one command, the connection closed when ctx ends first and kept from further use after a failure that leaves the protocol out of step
func (s *session) run(ctx context.Context, fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return fmt.Errorf("%s: %w", s.proto, net.ErrClosed)
	}
	if s.broken != nil {
		return s.broken
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", s.proto, err)
	}
	stop := context.AfterFunc(ctx, func() { _ = s.raw.Close() })
	err := fn()
	if !stop() {
		s.broken = fmt.Errorf("%s: connection abandoned: %w", s.proto, ctx.Err())
		return s.broken
	}
	if err != nil && !keepsSession(err) {
		if s.broken == nil {
			s.broken = err
		}
		_ = s.raw.Close()
	}
	return err
}

// Shuts the connection at once, interrupting a command in progress; doing it again does nothing
func (s *session) Close() error {
	if s.closed.Swap(true) || s.raw == nil {
		return nil
	}
	if err := s.raw.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("%s: %w", s.proto, err)
	}
	return nil
}

// Next line from the server without its line end, an error for a line longer than limit
func (s *session) readLine(limit int) (string, error) {
	var long []byte
	for {
		chunk, err := s.r.ReadSlice('\n')
		if len(long)+len(chunk) > limit {
			return "", fmt.Errorf("%s: server sent a line longer than %d bytes", s.proto, limit)
		}
		switch {
		case err == nil:
			if long != nil {
				chunk = append(long, chunk...)
			}
			return string(trimEOL(chunk)), nil
		case errors.Is(err, bufio.ErrBufferFull):
			long = append(long, chunk...)
		case errors.Is(err, io.EOF):
			if len(long)+len(chunk) > 0 {
				return "", fmt.Errorf("%s: connection closed in the middle of a line: %w", s.proto, io.ErrUnexpectedEOF)
			}
			return "", fmt.Errorf("%s: connection closed by the server: %w", s.proto, io.EOF)
		default:
			return "", fmt.Errorf("%s: %w", s.proto, err)
		}
	}
}

// Sends what is buffered
func (s *session) flush() error {
	if err := s.w.Flush(); err != nil {
		return fmt.Errorf("%s: %w", s.proto, err)
	}
	return nil
}

// Error for text that would break the command line it is written into
func (s *session) checkLine(what, text string) error {
	if strings.ContainsAny(text, "\r\n\x00") {
		return fmt.Errorf("%s: %w: %s contains a line break or NUL", s.proto, errArgument, what)
	}
	return nil
}

// Bytes without one trailing CRLF, LF or CR
func trimEOL(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	if n := len(b); n > 0 && b[n-1] == '\r' {
		b = b[:n-1]
	}
	return b
}
