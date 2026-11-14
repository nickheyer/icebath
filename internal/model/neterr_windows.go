package model

import (
	"errors"

	"golang.org/x/sys/windows"
)

func resetOrAborted(err error) bool {
	return errors.Is(err, windows.WSAECONNRESET) || errors.Is(err, windows.WSAECONNABORTED)
}

func brokenPipe(err error) bool {
	return errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.WSAESHUTDOWN)
}

func networkUnreachable(err error) bool {
	return errors.Is(err, windows.WSAENETUNREACH)
}
