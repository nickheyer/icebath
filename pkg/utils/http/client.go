package httputil

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/netpref"
)

// Redirects a client following them takes before it gives up
const MaxRedirects = 30

// Redirect chain longer than MaxRedirects
var ErrTooManyRedirects = errors.New("stopped after " + strconv.Itoa(MaxRedirects) + " redirects")

// Redirect policy following up to MaxRedirects hops, the Authorization, Proxy-Authorization and Cookie headers the first request carried dropped from every hop once the chain has left that origin (an upgrade of the same host from http on port 80 to https on port 443 not counting); or handing back each redirect response itself when follow is false
func RedirectPolicy(follow bool) func(*http.Request, []*http.Request) error {
	if !follow {
		return keepRedirect
	}
	return followRedirect
}

func keepRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func followRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > MaxRedirects {
		return ErrTooManyRedirects
	}
	// The client copies the first request's headers into every hop before this runs and adds jar cookies after it, so the whole chain is judged at each hop
	next := req.URL
	for _, prev := range slices.Backward(via) {
		if !sameOriginOrUpgrade(prev.URL, next) {
			for _, name := range credentialHeaders {
				req.Header.Del(name)
			}
			break
		}
		next = prev.URL
	}
	return nil
}

// Request headers holding credentials meant for the origin they were first sent to
var credentialHeaders = [...]string{"Authorization", "Proxy-Authorization", "Cookie", "Cookie2"}

// Whether a hop from one URL to the next stays on the same scheme, host and port, or upgrades the same host from http on port 80 to https on port 443
func sameOriginOrUpgrade(from, to *url.URL) bool {
	if from == nil || to == nil || !strings.EqualFold(from.Hostname(), to.Hostname()) {
		return false
	}
	fromScheme, toScheme := strings.ToLower(from.Scheme), strings.ToLower(to.Scheme)
	fromPort, toPort := effectivePort(fromScheme, from.Port()), effectivePort(toScheme, to.Port())
	if fromScheme == toScheme {
		return fromPort == toPort
	}
	return fromScheme == "http" && fromPort == "80" && toScheme == "https" && toPort == "443"
}

// Port of a URL with the default of http, https, ws and wss filled in
func effectivePort(scheme, port string) string {
	if port != "" {
		return strings.TrimLeft(port, "0")
	}
	switch scheme {
	case "http", "ws":
		return "80"
	case "https", "wss":
		return "443"
	}
	return ""
}

// Client sending through Transport, following redirects only when follow is true
func Client(follow bool) *http.Client {
	return &http.Client{Transport: Transport(), CheckRedirect: RedirectPolicy(follow)}
}

// Process wide transport: the settings of http.DefaultTransport with the proxy Proxy picks for each request and dials following the address family preference, connections pooled across every caller
func Transport() http.RoundTripper { return sharedTransport }

var sharedTransport = &proxyTransport{}

// Sends through a clone of http.DefaultTransport carrying Proxy, cloned again when the default is replaced, a default that is not an *http.Transport used as it is
type proxyTransport struct {
	mu    sync.Mutex
	base  *http.Transport
	clone *http.Transport
}

func (p *proxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport.RoundTrip(req)
	}
	p.mu.Lock()
	if p.base != base {
		if p.clone != nil {
			p.clone.CloseIdleConnections()
		}
		clone := base.Clone()
		clone.Proxy = Proxy
		p.base, p.clone = base, netpref.Follow(clone)
	}
	t := p.clone
	p.mu.Unlock()
	return t.RoundTrip(req)
}

// Closes the idle connections of the shared clone
func (p *proxyTransport) CloseIdleConnections() {
	p.mu.Lock()
	t := p.clone
	p.mu.Unlock()
	if t != nil {
		t.CloseIdleConnections()
	}
}
