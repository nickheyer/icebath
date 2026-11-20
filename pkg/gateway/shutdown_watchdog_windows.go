//go:build windows

package gateway

import (
	"context"
	"net"
)

// Windows never binds AF_UNIX for the tick witness, the TCP loopback listener serves it
func listenTickUnix(context.Context, string) (net.Listener, string, bool, error) {
	return nil, "", false, nil
}
