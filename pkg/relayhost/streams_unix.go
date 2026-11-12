//go:build !windows

package relayhost

import (
	"os"

	"golang.org/x/sys/unix"
)

// Duplicates stdin and stdout for the channel, then points fd 0 at the null device and fd 1 at stderr
func takeProtocolStreams() (*os.File, *os.File, error) {
	in, err := unix.Dup(0)
	if err != nil {
		return nil, nil, err
	}
	out, err := unix.Dup(1)
	if err != nil {
		return nil, nil, err
	}
	null, err := unix.Open(os.DevNull, unix.O_RDONLY, 0)
	if err != nil {
		return nil, nil, err
	}
	if err := unix.Dup2(null, 0); err != nil {
		return nil, nil, err
	}
	_ = unix.Close(null)
	if err := unix.Dup2(2, 1); err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(in), "relay-host-in"), os.NewFile(uintptr(out), "relay-host-out"), nil
}
