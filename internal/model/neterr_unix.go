//go:build !windows

package model

import (
	"errors"
	"syscall"
)

func resetOrAborted(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED)
}

func brokenPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE)
}

func networkUnreachable(err error) bool {
	return errors.Is(err, syscall.ENETUNREACH)
}
