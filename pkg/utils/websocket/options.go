package websocketutil

import (
	"crypto/tls"
	"net/http"
	"net/url"
	"time"
)

// Defaults the zero fields of Options take
const (
	// Limit on the opening handshake
	DefaultOpenTimeout = 30 * time.Second
	// Wait for the peer's close frame
	DefaultCloseTimeout = 10 * time.Second
	// Silence from the peer before a keepalive ping
	DefaultPingInterval = 20 * time.Second
	// Largest message accepted, in bytes
	DefaultMaxMessageSize = 4 << 20
)

// Wait a control frame gets for its write when Options sets no write timeout
const controlWriteWait = 10 * time.Second

// Settings of one connection; a zero duration or size takes its default and a negative one switches the limit off, except WriteTimeout, which has no default
type Options struct {
	// Limit on the name lookup, dial, proxy, TLS handshake and upgrade together, DefaultOpenTimeout when zero, none besides the context when negative
	OpenTimeout time.Duration
	// Wait for the peer's close frame once this side sent its own, counted from the start of the close, DefaultCloseTimeout when zero, none at all when negative so a close drops the connection right after sending
	CloseTimeout time.Duration
	// Silence from the peer after which a keepalive ping goes out, DefaultPingInterval when zero, no keepalive pings when negative; time in which Receive is not taking messages and the reader waits does not count as silence
	PingInterval time.Duration
	// Wait for anything at all from the peer once a keepalive ping went out, and the limit on writing that ping, before the connection fails with CloseInternalError and ErrKeepaliveTimeout, the ping interval when zero or negative
	PingTimeout time.Duration
	// Limit on handing one whole message to the network, counted from the start of the send; none when zero or negative, the default, because a large message over a slow link can take any time; a send that runs out of it fails the connection, and without it a send held up by a peer that stops reading waits until the keepalive, the reader or Close ends the connection
	WriteTimeout time.Duration
	// Largest message accepted in bytes, counted after decompression, DefaultMaxMessageSize when zero, no limit when negative; a bigger one fails the connection with CloseMessageTooBig and ErrMessageTooBig
	MaxMessageSize int64
	// Extra headers of the upgrade request such as Origin, User-Agent, Authorization, Cookie or Sec-WebSocket-Protocol
	Header http.Header
	// Subprotocols offered in order of preference; a server choosing one offered neither here nor in a Sec-WebSocket-Protocol header fails the handshake, Conn.Subprotocol gives its choice
	Subprotocols []string
	// Proxy for the connection, nil to connect directly: httputil.Proxy follows the environment and then the operating system settings, http.ProxyURL pins one; http, https, socks5 and socks5h proxies work
	Proxy func(*http.Request) (*url.URL, error)
	// TLS settings of a wss connection, the defaults when nil; application protocols other than http/1.1 are dropped from it
	TLSConfig *tls.Config
	// Offer permessage-deflate compression without context takeover; a server that accepts it without granting client_no_context_takeover fails the handshake with a *HandshakeError of status 101, since gorilla/websocket cannot keep a compression context
	Compression bool
	// Wait before racing a dial over the other address family, net.Dialer.FallbackDelay: 300ms when zero, no racing when negative
	FallbackDelay time.Duration
}

// Limits of Options with the defaults applied, a value of zero or below meaning off
type limits struct {
	openTimeout    time.Duration
	closeTimeout   time.Duration
	pingInterval   time.Duration
	pingTimeout    time.Duration
	writeTimeout   time.Duration
	maxMessageSize int64
}

func (o *Options) resolve() limits {
	l := limits{
		openTimeout:    orDefault(o.OpenTimeout, DefaultOpenTimeout),
		closeTimeout:   orDefault(o.CloseTimeout, DefaultCloseTimeout),
		pingInterval:   orDefault(o.PingInterval, DefaultPingInterval),
		pingTimeout:    o.PingTimeout,
		writeTimeout:   o.WriteTimeout,
		maxMessageSize: orDefault(o.MaxMessageSize, DefaultMaxMessageSize),
	}
	if l.pingTimeout <= 0 {
		l.pingTimeout = l.pingInterval
	}
	return l
}

func orDefault[T time.Duration | int64](v, def T) T {
	if v == 0 {
		return def
	}
	return v
}

// Wait every control frame gets for its write: the write timeout when one is set, else controlWriteWait, so a ping, pong or close never blocks for good
func (l limits) controlWait() time.Duration {
	if l.writeTimeout > 0 {
		return l.writeTimeout
	}
	return controlWriteWait
}
