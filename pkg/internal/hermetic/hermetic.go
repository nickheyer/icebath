package hermetic

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

var blockOnce sync.Once

// Whether a dial host names this machine, by name or by a loopback address
func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// BlockExternalHTTP makes the default transport of a test binary, and every clone of it, dial loopback hosts only
func BlockExternalHTTP() {
	blockOnce.Do(func() {
		transport, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			panic(fmt.Sprintf("hermetic: the default transport is a %T", http.DefaultTransport))
		}
		dial := (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil || !loopbackHost(host) {
				return nil, fmt.Errorf("network disabled in tests: %s", addr)
			}
			return dial(ctx, network, addr)
		}
	})
}
