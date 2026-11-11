package dashboard

import (
	"context"
	"net/netip"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyipaddress"
)

// Hosts trusted to set forwarding headers, the uvicorn _TrustedHosts of IPs, networks and literals
type trustedHosts struct {
	alwaysTrust bool
	hosts       map[netip.Addr]bool
	networks    []netip.Prefix
	literals    map[string]bool
}

// Trusted set of a forwarded_allow_ips list, a star trusting every peer
func newTrustedHosts(entries []string) *trustedHosts {
	t := &trustedHosts{hosts: map[netip.Addr]bool{}, literals: map[string]bool{}}
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "*" {
			t.alwaysTrust = true
			continue
		}
		if strings.Contains(e, "/") {
			if n, ok := pyipaddress.IPNetwork(e, true); ok {
				t.networks = append(t.networks, n)
			} else {
				t.literals[e] = true
			}
			continue
		}
		if a, ok := pyipaddress.IPAddress(e); ok {
			t.hosts[a] = true
		} else {
			t.literals[e] = true
		}
	}
	return t
}

// Whether a host is trusted by address, network or literal name
func (t *trustedHosts) contains(host string) bool {
	if t.alwaysTrust {
		return true
	}
	a, ok := pyipaddress.IPAddress(host)
	if !ok {
		return t.literals[host]
	}
	if t.hosts[a] {
		return true
	}
	for _, n := range t.networks {
		if pyipaddress.NetworkContains(n, a) {
			return true
		}
	}
	return false
}

// Client host of an X-Forwarded-For list, the nearest untrusted hop or the first when all are trusted
func (t *trustedHosts) trustedClientHost(xff string) string {
	var hosts []string
	for _, h := range strings.Split(xff, ",") {
		hosts = append(hosts, strings.TrimSpace(h))
	}
	if t.alwaysTrust {
		return hosts[0]
	}
	for i := len(hosts) - 1; i >= 0; i-- {
		if !t.contains(hosts[i]) {
			return hosts[i]
		}
	}
	return hosts[0]
}

// Rewrites scheme and client from X-Forwarded-Proto and X-Forwarded-For when the peer is trusted
func proxyHeadersMiddleware(app ASGIApp, trusted *trustedHosts) ASGIApp {
	return func(ctx context.Context, scope *Scope, receive Receive, send Send) error {
		if scope.Type != "http" && scope.Type != "websocket" {
			return app(ctx, scope, receive, send)
		}
		if scope.Client == nil || !trusted.contains(scope.Client.Host) {
			return app(ctx, scope, receive, send)
		}
		headers := map[string]string{}
		for _, h := range scope.Headers {
			headers[h[0]] = h[1]
		}
		if proto, ok := headers["x-forwarded-proto"]; ok {
			proto = strings.TrimSpace(latin1Decode(proto))
			switch proto {
			case "http", "https", "ws", "wss":
				if scope.Type == "websocket" {
					scope.Scheme = strings.Replace(proto, "http", "ws", 1)
				} else {
					scope.Scheme = proto
				}
			}
		}
		if xff, ok := headers["x-forwarded-for"]; ok {
			if host := trusted.trustedClientHost(latin1Decode(xff)); host != "" {
				scope.Client = &Address{Host: host, Port: 0}
			}
		}
		return app(ctx, scope, receive, send)
	}
}
