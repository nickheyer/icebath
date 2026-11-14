//go:build unix

package terminal

import (
	"os"
	"syscall"
)

func openNonblocking(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
