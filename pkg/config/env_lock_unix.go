//go:build !windows

package config

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// Takes the cross process .env write lock, failing once the timeout passes
func lockEnvFile(path string, timeout time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return func() {}, nil
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("Timed out waiting for the environment file lock (%s)", path[:len(path)-len(".lock")])
		}
		time.Sleep(50 * time.Millisecond)
	}
}
