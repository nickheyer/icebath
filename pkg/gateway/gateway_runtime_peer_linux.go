package gateway

import (
	"net"

	"golang.org/x/sys/unix"
)

// Kernel reported uid of the peer of a connected unix socket through SO_PEERCRED
func socketPeerUID(conn net.Conn) (int, bool, error) {
	raw, err := conn.(*net.UnixConn).SyscallConn()
	if err != nil {
		return 0, false, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) { cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return 0, false, err
	}
	if credErr != nil {
		return 0, false, credErr
	}
	return int(cred.Uid), true, nil
}
