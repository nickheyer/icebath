//go:build !windows

package cliui

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// Readable wait on a file descriptor that a write to its wake pipe ends
type fdGate struct {
	fd           int
	wakeR, wakeW int
}

// Gate of a file backed stream, nil for any other reader or when no wake pipe can be made
func newReadGate(r io.Reader) readGate {
	f, isFile := r.(*os.File)
	if !isFile {
		return nil
	}
	var pipe [2]int
	if err := unix.Pipe(pipe[:]); err != nil {
		return nil
	}
	for _, end := range pipe {
		unix.CloseOnExec(end)
		if err := unix.SetNonblock(end, true); err != nil {
			unix.Close(pipe[0])
			unix.Close(pipe[1])
			return nil
		}
	}
	return &fdGate{fd: int(f.Fd()), wakeR: pipe[0], wakeW: pipe[1]}
}

func (g *fdGate) wait() bool {
	for {
		fds := []unix.PollFd{{Fd: int32(g.fd), Events: unix.POLLIN}, {Fd: int32(g.wakeR), Events: unix.POLLIN}}
		if _, err := unix.Poll(fds, -1); err != nil {
			if err == unix.EINTR {
				continue
			}
			return true
		}
		if fds[0].Revents != 0 {
			return true
		}
		if fds[1].Revents != 0 {
			var drained [64]byte
			for {
				if n, err := unix.Read(g.wakeR, drained[:]); n <= 0 || err != nil {
					break
				}
			}
			return false
		}
	}
}

func (g *fdGate) release() {
	_, _ = unix.Write(g.wakeW, []byte{0})
}
