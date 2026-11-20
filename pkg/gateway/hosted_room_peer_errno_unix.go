//go:build !windows

package gateway

import (
	"errors"
	"syscall"
)

// Whether the connection failed with an errno proving the request never reached the peer
func peerNotAdmittedErrno(err error) bool {
	for _, errno := range []syscall.Errno{syscall.ECONNREFUSED, syscall.ENETDOWN, syscall.ENETUNREACH, syscall.EHOSTDOWN, syscall.EHOSTUNREACH} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}
