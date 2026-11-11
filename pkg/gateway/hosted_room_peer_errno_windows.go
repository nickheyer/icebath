//go:build windows

package gateway

import (
	"errors"
	"syscall"
)

// Winsock errnos Python's errno module names ECONNREFUSED, ENETDOWN, ENETUNREACH, EHOSTDOWN and EHOSTUNREACH
var peerNotAdmittedWinsock = []syscall.Errno{10061, 10050, 10051, 10064, 10065}

// Whether the connection failed with an errno proving the request never reached the peer
func peerNotAdmittedErrno(err error) bool {
	for _, errno := range peerNotAdmittedWinsock {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}
