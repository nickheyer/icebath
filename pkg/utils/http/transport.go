package httputil

import (
	"context"
	"net/http"
)

// Transport holding each request to the phase limits in t, the read limit restarting with every body read and not running between reads; a request that runs out of a limit fails with a *TimeoutError
func TimeoutTransport(base http.RoundTripper, t Timeout) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &timeoutTransport{base: base, limits: t}
}

type timeoutTransport struct {
	base   http.RoundTripper
	limits Timeout
}

func (t *timeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	w := newWatchdog(t.limits, cancel)
	resp, err := t.base.RoundTrip(w.request(ctx, req))
	if err != nil {
		w.stop()
		cancel()
		if te := w.timedOut(); te != nil {
			return nil, te
		}
		return nil, err
	}
	w.respond()
	resp.Body = &watchedBody{rc: resp.Body, w: w, cancel: cancel}
	return resp, nil
}

// Drops the idle connections of the transport underneath
func (t *timeoutTransport) CloseIdleConnections() { closeIdle(t.base) }

// Content codings DecodingTransport asks for
const acceptedCodings = "gzip, deflate, br, zstd"

// Transport asking for gzip, deflate, br and zstd content when a request names no Accept-Encoding of its own, handing back every response body with those codings undone and its Content-Encoding and Content-Length removed, and sending no Authorization or Proxy-Authorization header on a redirect hop once the chain has left the origin of its first request (an upgrade of the same host from http on port 80 to https on port 443 not counting), whatever redirect policy the client follows
func DecodingTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &decodingTransport{base: base}
}

type decodingTransport struct{ base http.RoundTripper }

func (t *decodingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req
	own := func() {
		if out == req {
			out = req.WithContext(req.Context())
			out.Header = req.Header.Clone()
			if out.Header == nil {
				out.Header = http.Header{}
			}
		}
	}
	if redirectLeftOrigin(req) && (req.Header.Get("Authorization") != "" || req.Header.Get("Proxy-Authorization") != "") {
		own()
		out.Header.Del("Authorization")
		out.Header.Del("Proxy-Authorization")
	}
	if req.Header.Get("Accept-Encoding") == "" && req.Header.Get("Range") == "" && req.Method != http.MethodHead {
		own()
		out.Header.Set("Accept-Encoding", acceptedCodings)
	}
	resp, err := t.base.RoundTrip(out)
	if err != nil {
		return resp, err
	}
	decodeResponse(resp)
	return resp, nil
}

// Drops the idle connections of the transport underneath
func (t *decodingTransport) CloseIdleConnections() { closeIdle(t.base) }

// Longest redirect chain read back before it counts as having left its origin, a bound against transports whose reused responses link a request back to itself
const maxChainWalk = 1000

// Whether the request is a redirect hop of a chain that moved to another origin at some hop, read back through the responses that caused each hop
func redirectLeftOrigin(req *http.Request) bool {
	r := req
	for range maxChainWalk {
		if r.Response == nil || r.Response.Request == nil {
			return false
		}
		prev := r.Response.Request
		if !sameOriginOrUpgrade(prev.URL, r.URL) {
			return true
		}
		r = prev
	}
	return true
}

func closeIdle(rt http.RoundTripper) {
	if c, ok := rt.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}
