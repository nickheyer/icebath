package websocketutil

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/utils/netpref"
)

// Opens a client connection to a ws, wss, http or https URL, user info in the URL sent as basic authorization; ctx bounds only the opening handshake, whose failure is ctx's own error, an ErrInvalidURL error, a *HandshakeError, an *OpenTimeoutError or a *ConnectError
func Connect(ctx context.Context, rawURL string, opts Options) (*Conn, error) {
	target, header, err := prepareURL(rawURL, opts.Header)
	if err != nil {
		return nil, err
	}
	shown := displayURL(target)
	lim := opts.resolve()
	var dctx context.Context
	var cancel context.CancelFunc
	if lim.openTimeout > 0 {
		dctx, cancel = context.WithTimeout(ctx, lim.openTimeout)
	} else {
		dctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()
	guard := &handshakeGuard{}
	act := newActivity()
	dial := guard.wrap(netpref.DialContext((&net.Dialer{FallbackDelay: opts.FallbackDelay}).DialContext))
	if lim.pingInterval > 0 {
		dial = act.wrap(dial)
	}
	dialer := websocket.Dialer{
		NetDialContext:    dial,
		Proxy:             opts.Proxy,
		TLSClientConfig:   http1TLSConfig(opts.TLSConfig),
		Subprotocols:      opts.Subprotocols,
		EnableCompression: opts.Compression,
	}
	stop := context.AfterFunc(dctx, guard.abort)
	ws, resp, err := dialer.DialContext(dctx, target.String(), header)
	if !stop() && err == nil {
		_ = ws.Close()
		err = dctx.Err()
	}
	if err != nil {
		return nil, connectFailure(ctx, dctx, lim.openTimeout, shown, resp, err)
	}
	if chosen := ws.Subprotocol(); chosen != "" && !slices.Contains(opts.Subprotocols, chosen) && !headerOffers(header, chosen) {
		_ = ws.Close()
		return nil, newHandshakeError(resp, fmt.Errorf("server chose the subprotocol %q, which was not offered", chosen), shown)
	}
	return newConn(ws, lim, act), nil
}

// Error of a failed opening handshake: ctx's own error once the caller's context ended, an *OpenTimeoutError with its Limit when the open timeout ran out and with its Err when the operating system or a name server timed out first
func connectFailure(ctx, dctx context.Context, limit time.Duration, shown string, resp *http.Response, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if resp != nil && errors.Is(err, websocket.ErrBadHandshake) {
		return newHandshakeError(resp, err, shown)
	}
	if expired(ctx) {
		return context.DeadlineExceeded
	}
	if dctx.Err() != nil || expired(dctx) {
		return &OpenTimeoutError{URL: shown, Limit: limit}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &OpenTimeoutError{URL: shown, Err: err}
	}
	if resp != nil && resp.StatusCode == http.StatusSwitchingProtocols {
		return newHandshakeError(resp, err, shown)
	}
	return &ConnectError{URL: shown, Err: err}
}

// Whether the context's deadline has passed, true a moment before its Err reports it when a network deadline set from it fired first
func expired(ctx context.Context) bool {
	deadline, ok := ctx.Deadline()
	return ok && !time.Now().Before(deadline)
}

// Whether a Sec-WebSocket-Protocol header among the extra headers offers the subprotocol, the header name matched in any case as gorilla/websocket sends it verbatim
func headerOffers(header http.Header, proto string) bool {
	for name, values := range header {
		if !strings.EqualFold(name, "Sec-WebSocket-Protocol") {
			continue
		}
		for _, v := range values {
			for p := range strings.SplitSeq(v, ",") {
				if strings.TrimSpace(p) == proto {
					return true
				}
			}
		}
	}
	return false
}

// URL to dial with a ws or wss scheme and no user info or fragment, and the request headers: a copy of extra with the URL's user info as basic authorization unless extra authorizes already
func prepareURL(rawURL string, extra http.Header) (*url.URL, http.Header, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "ws", "http":
		u.Scheme = "ws"
	case "wss", "https":
		u.Scheme = "wss"
	default:
		return nil, nil, fmt.Errorf("%w: the scheme %q is not ws, wss, http or https", ErrInvalidURL, u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, nil, fmt.Errorf("%w: the URL names no host", ErrInvalidURL)
	}
	u.Fragment, u.RawFragment = "", ""
	header := extra.Clone()
	if u.User != nil {
		if header == nil {
			header = http.Header{}
		}
		if header.Get("Authorization") == "" {
			password, _ := u.User.Password()
			header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+password)))
		}
		u.User = nil
	}
	return u, header, nil
}

// TLS settings limited to HTTP/1.1, the only application protocol a WebSocket upgrade runs over, cfg itself when it offers no other
func http1TLSConfig(cfg *tls.Config) *tls.Config {
	notHTTP1 := func(proto string) bool { return proto != "http/1.1" }
	if cfg == nil || !slices.ContainsFunc(cfg.NextProtos, notHTTP1) {
		return cfg
	}
	out := cfg.Clone()
	out.NextProtos = slices.DeleteFunc(slices.Clone(cfg.NextProtos), notHTTP1)
	return out
}

// Network connection of one opening handshake, closed when the handshake's context ends; the dialer honours a context's deadline while it writes the upgrade and reads the answer but not its cancellation
type handshakeGuard struct {
	mu      sync.Mutex
	conn    net.Conn
	aborted bool
}

// Dial function recording the connection it opens, closing it at once when the handshake was already aborted
func (g *handshakeGuard) wrap(dial func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		g.mu.Lock()
		aborted := g.aborted
		if !aborted {
			g.conn = conn
		}
		g.mu.Unlock()
		if aborted {
			_ = conn.Close()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, net.ErrClosed
		}
		return conn, nil
	}
}

func (g *handshakeGuard) abort() {
	g.mu.Lock()
	g.aborted = true
	conn := g.conn
	g.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}
