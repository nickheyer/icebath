//go:build !windows

package gateway

import (
	"errors"
	"time"
)

var errNativeWindowsOnly = errors.New("native Windows bootstrap requires Windows")

// Native pipe client exists only on Windows, any other host raises like the missing winapi module
func queryRuntimeControl(home string, request []byte, timeout time.Duration) ([]byte, error) {
	return nil, errNativeWindowsOnly
}

// Native pipe instances exist only on Windows, any other host raises like the missing winapi module
func createNativePipe(home string, first bool) (nativePipeInstance, error) {
	return nil, errNativeWindowsOnly
}
