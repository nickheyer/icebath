//go:build unix

package vsc

import (
	"context"
	"io"
	"net"
)

// Connects to the unix socket at the path
func dial(ctx context.Context, path string) (io.ReadWriteCloser, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}
