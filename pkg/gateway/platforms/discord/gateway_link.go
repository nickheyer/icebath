package discord

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Most seconds the command sync lets one rate limited request sleep before raising
const commandSyncMaxRateLimitSleepSeconds = 30.0

// Close codes discord.py surfaces as ConnectionClosed instead of resuming
var fatalCloseCodes = map[int]bool{1000: true, 4004: true, 4010: true, 4011: true, 4012: true, 4013: true, 4014: true}

// Raised when the gateway socket closes with a code that cannot be resumed
type connectionClosed struct {
	shardID *int
	code    int
}

func (e *connectionClosed) Error() string {
	shard := "None"
	if e.shardID != nil {
		shard = fmt.Sprint(*e.shardID)
	}
	return fmt.Sprintf("Shard ID %s WebSocket closed with %d", shard, e.code)
}

// Gateway client of the adapter, the discordgo session plus the socket and close state it hides
type gatewayLink struct {
	*discordgo.Session
	mu           sync.Mutex
	closed       bool
	ws           *wsTransport
	conn         *watchConn
	disconnected chan struct{}
	rateCap      *rateLimitCap
}

// Builds the bot client with the intents, allowed mentions and proxy of one connect
var commandsBot = newGatewayLink

func newGatewayLink(token string, intents discordgo.Intent, allowedMentions *discordgo.MessageAllowedMentions, proxy gateway.ProxyKwargs) (*gatewayLink, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	s.Identify.Intents = intents
	s.ShouldReconnectOnError = false
	s.State.MaxMessageCount = 1000
	s.LogLevel = discordgo.LogError
	dialer := *s.Dialer
	s.Dialer = &dialer
	l := &gatewayLink{Session: s, disconnected: make(chan struct{}, 1)}
	base := &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true, TLSHandshakeTimeout: 15 * time.Second}
	dial := (&net.Dialer{Timeout: 30 * time.Second}).DialContext
	switch {
	case proxy.Connector != nil:
		base = proxy.Connector.Transport()
		dial = proxy.Connector.DialContext
	case proxy.Proxy != "":
		u, err := url.Parse(proxy.Proxy)
		if err != nil {
			return nil, err
		}
		base.Proxy = http.ProxyURL(u)
		s.Dialer.Proxy = http.ProxyURL(u)
	}
	l.rateCap = &rateLimitCap{base: base}
	s.Client = &http.Client{Timeout: 20 * time.Second, Transport: l.rateCap}
	s.Dialer.NetDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return l.watch(c), nil
	}
	s.Dialer.NetDialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		raw, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		host, _, _ := net.SplitHostPort(addr)
		tc := tls.Client(raw, &tls.Config{ServerName: host})
		if err := tc.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, err
		}
		return l.watch(tc), nil
	}
	s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Disconnect) {
		select {
		case l.disconnected <- struct{}{}:
		default:
		}
	})
	return l, nil
}

// Records the socket of a fresh gateway connection
func (l *gatewayLink) watch(c net.Conn) net.Conn {
	w := &watchConn{Conn: c}
	w.open.Store(true)
	l.mu.Lock()
	l.conn, l.ws = w, &wsTransport{conn: w}
	l.mu.Unlock()
	return w
}

// Whether the client was closed on purpose
func (l *gatewayLink) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// Closes the client for good, ending its connect loop
func (l *gatewayLink) close() error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	return l.Session.Close()
}

// Socket of the current connection, nil before the first dial
func (l *gatewayLink) socket() *watchConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn
}

// Server close code of the last connection, zero when the socket ended without one
func (l *gatewayLink) lastCloseCode() int {
	if c := l.socket(); c != nil {
		return int(c.closeCode.Load())
	}
	return 0
}

// Exponential backoff of the discord.py reconnect loop, uniform jitter up to two to the attempt
type exponentialBackoff struct {
	exp            int
	lastInvocation time.Time
}

func (b *exponentialBackoff) delay() time.Duration {
	now := time.Now()
	if now.Sub(b.lastInvocation) > (1<<11)*time.Second {
		b.exp = 0
	}
	b.lastInvocation = now
	b.exp = min(b.exp+1, 10)
	return time.Duration(rand.Float64() * math.Pow(2, float64(b.exp)) * float64(time.Second))
}

// Logs in then keeps the gateway connected the way discord.py Client.start does until closed
func (l *gatewayLink) start(ctx context.Context, token string) error {
	if _, err := l.User("@me", discordgo.WithContext(ctx)); err != nil {
		var rest *discordgo.RESTError
		if errors.As(err, &rest) && rest.Response != nil && rest.Response.StatusCode == http.StatusUnauthorized {
			return &loginFailure{msg: "Improper token has been passed."}
		}
		return dpyError(err)
	}
	backoff := &exponentialBackoff{lastInvocation: time.Now()}
	for !l.isClosed() {
		select {
		case <-l.disconnected:
		default:
		}
		l.mu.Lock()
		l.conn = nil
		l.mu.Unlock()
		openErr := l.openWithin(ctx, 60*time.Second)
		if openErr == nil {
			select {
			case <-l.disconnected:
			case <-ctx.Done():
			}
		}
		if ctx.Err() != nil {
			l.close()
			return ctx.Err()
		}
		if l.isClosed() {
			return nil
		}
		code := l.lastCloseCode()
		switch {
		case code == 4014:
			return &privilegedIntentsRequired{}
		case code != 1000 && fatalCloseCodes[code]:
			l.close()
			return &connectionClosed{code: code}
		case code != 0 || openErr == nil:
			continue
		}
		retry := backoff.delay()
		slog.Error(fmt.Sprintf("Attempting a reconnect in %.2fs: %s", retry.Seconds(), openErr))
		select {
		case <-time.After(retry):
		case <-ctx.Done():
			l.close()
			return ctx.Err()
		}
	}
	return nil
}

// Opens one gateway connection, resuming when a session exists, bounded like discord.py's 60 second wait
func (l *gatewayLink) openWithin(ctx context.Context, limit time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- l.Session.Open() }()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		if c := l.socket(); c != nil {
			c.Close()
		}
		return errors.New("TimeoutError")
	case <-ctx.Done():
		if c := l.socket(); c != nil {
			c.Close()
		}
		return ctx.Err()
	}
}

// Socket wrapper reading server close codes from the websocket frame stream
type watchConn struct {
	net.Conn
	open      atomic.Bool
	closeCode atomic.Int32
	mu        sync.Mutex
	hdr       []byte
	remaining uint64
	closing   bool
	payload   []byte
	upgraded  bool
	crlf      int
}

func (w *watchConn) Read(p []byte) (int, error) {
	n, err := w.Conn.Read(p)
	if n > 0 {
		w.scan(p[:n])
	}
	if err != nil {
		w.open.Store(false)
	}
	return n, err
}

func (w *watchConn) Close() error {
	w.open.Store(false)
	return w.Conn.Close()
}

// Skips the HTTP upgrade response, then parses frames and records the code of a server close frame
func (w *watchConn) scan(b []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for !w.upgraded && len(b) > 0 {
		if b[0] == "\r\n\r\n"[w.crlf] {
			w.crlf++
		} else if b[0] == '\r' {
			w.crlf = 1
		} else {
			w.crlf = 0
		}
		b = b[1:]
		w.upgraded = w.crlf == 4
	}
	for len(b) > 0 {
		if w.remaining > 0 {
			take := min(uint64(len(b)), w.remaining)
			if w.closing && len(w.payload) < 2 {
				w.payload = append(w.payload, b[:min(take, uint64(2-len(w.payload)))]...)
			}
			w.remaining -= take
			b = b[take:]
			if w.remaining == 0 {
				w.finishFrame()
			}
			continue
		}
		w.hdr = append(w.hdr, b[0])
		b = b[1:]
		need := 2
		if len(w.hdr) >= 2 {
			switch w.hdr[1] & 0x7f {
			case 126:
				need = 4
			case 127:
				need = 10
			}
			if w.hdr[1]&0x80 != 0 {
				need += 4
			}
		}
		if len(w.hdr) < need {
			continue
		}
		size := uint64(w.hdr[1] & 0x7f)
		switch size {
		case 126:
			size = uint64(binary.BigEndian.Uint16(w.hdr[2:4]))
		case 127:
			size = binary.BigEndian.Uint64(w.hdr[2:10])
		}
		w.closing = w.hdr[0]&0x0f == websocket.CloseMessage
		w.payload = w.payload[:0]
		w.hdr = w.hdr[:0]
		w.remaining = size
		if size == 0 {
			w.finishFrame()
		}
	}
}

// Stores the close code of a completed close frame
func (w *watchConn) finishFrame() {
	if !w.closing {
		return
	}
	if len(w.payload) >= 2 {
		w.closeCode.Store(int32(binary.BigEndian.Uint16(w.payload[:2])))
	} else {
		w.closeCode.Store(int32(websocket.CloseNoStatusReceived))
	}
	w.closing = false
}

// REST transport that raises a rate limit error instead of sleeping past the configured cap
type rateLimitCap struct {
	base    http.RoundTripper
	maxBits atomic.Uint64
}

// Current cap in seconds, zero meaning unbounded
func (r *rateLimitCap) max() float64 { return math.Float64frombits(r.maxBits.Load()) }

// Replaces the cap and returns the previous one
func (r *rateLimitCap) swap(seconds float64) float64 {
	return math.Float64frombits(r.maxBits.Swap(math.Float64bits(seconds)))
}

func (r *rateLimitCap) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.base.RoundTrip(req)
	limit := r.max()
	if err != nil || resp.StatusCode != http.StatusTooManyRequests || limit <= 0 {
		return resp, err
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	var rl discordgo.TooManyRequests
	if err := discordgo.Unmarshal(body, &rl); err != nil || rl.RetryAfter.Seconds() <= limit {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, nil
	}
	return nil, &discordgo.RateLimitError{RateLimit: &discordgo.RateLimit{TooManyRequests: &rl, URL: req.URL.String()}}
}
