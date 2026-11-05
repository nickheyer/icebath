//go:build !windows

package tools

import (
	"errors"
	"os"
	"syscall"
)

// Takes an exclusive flock without waiting, false when another holder has it
func checkpointTryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return false, err
}
