//go:build !windows

package gateway

import (
	"errors"
	"io"
	"time"
)

// One private control socket exchange with the owner of home, the response line or nil
func ownerControlExchange(home string, request []byte, deadline time.Time) ([]byte, error) {
	peer, err := connectPrivate(home, time.Until(deadline))
	if err != nil {
		return nil, err
	}
	defer peer.Close()
	if _, err := peer.Write(request); err != nil {
		return nil, err
	}
	buf := make([]byte, 65536)
	return readResponseLine(func() ([]byte, error) {
		wait := time.Until(deadline)
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		if err := peer.SetReadDeadline(time.Now().Add(wait)); err != nil {
			return nil, err
		}
		n, err := peer.Read(buf)
		if n == 0 && err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		return append([]byte(nil), buf[:n]...), nil
	}, deadline)
}
