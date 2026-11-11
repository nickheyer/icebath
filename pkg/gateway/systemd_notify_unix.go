//go:build !windows

package gateway

import (
	"strings"

	"golang.org/x/sys/unix"
)

// Writes one datagram to a unix socket without blocking, a leading at sign naming an abstract socket
func systemdNotifySend(address, message string) bool {
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	unix.CloseOnExec(fd)
	if err := unix.SetNonblock(fd, true); err != nil {
		return false
	}
	name := address
	if strings.HasPrefix(address, "@") {
		name = "\x00" + address[1:]
	}
	if err := unix.Connect(fd, &unix.SockaddrUnix{Name: name}); err != nil {
		return false
	}
	_, err = unix.Write(fd, []byte(message))
	return err == nil
}
