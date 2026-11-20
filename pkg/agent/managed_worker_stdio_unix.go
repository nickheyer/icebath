//go:build !windows

package agent

import (
	"os"

	"golang.org/x/sys/unix"
)

// Private duplicate of stdout as the frame channel, stdout itself then pointing at stderr so stray prints never corrupt it
func managedWorkerStdio() (*os.File, error) {
	fd, err := unix.FcntlInt(1, unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Dup2(2, 1); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "managed-worker-channel"), nil
}
