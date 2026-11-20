//go:build !linux && !darwin && !windows

package gateway

import "net"

// Platforms without a peer credential API rely on the path checks alone
func socketPeerUID(conn net.Conn) (int, bool, error) { return 0, false, nil }
