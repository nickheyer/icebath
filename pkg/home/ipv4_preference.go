package home

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
)

// Dial function shape of net.Dialer and http.Transport
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

var ipv4Preferred atomic.Bool

// Reports whether the process prefers IPv4 for connections that name no address family
func IPv4Preferred() bool { return ipv4Preferred.Load() }

// Makes the default HTTP transport and every clone made after it resolve unspecified families as IPv4 first
func ApplyIPv4Preference(force bool) {
	if !force || ipv4Preferred.Swap(true) {
		return
	}
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		dial := DialFunc(t.DialContext)
		if t.DialContext == nil {
			dial = (&net.Dialer{}).DialContext
		}
		t.DialContext = PreferIPv4(dial)
	}
}

// Wraps dial so tcp and udp dials use the host's A records, falling back to full resolution when it has none
func PreferIPv4(dial DialFunc) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" && network != "udp" {
			return dial(ctx, network, addr)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return dial(ctx, network, addr)
		}
		ips, err := lookupIPv4(ctx, host)
		if err != nil || len(ips) == 0 {
			return dial(ctx, network, addr)
		}
		var first error
		for _, ip := range ips {
			conn, err := dial(ctx, network+"4", net.JoinHostPort(ip, port))
			if err == nil {
				return conn, nil
			}
			if first == nil {
				first = err
			}
		}
		return nil, first
	}
}

// IPv4 addresses of host in resolver order, swapped by tests
var lookupIPv4 = func(ctx context.Context, host string) ([]string, error) {
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.Unmap().String())
	}
	return out, nil
}
