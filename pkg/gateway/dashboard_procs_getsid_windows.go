//go:build windows

package gateway

import "golang.org/x/sys/windows"

// Sessions of the POSIX kind do not exist on Windows so every lookup is the unsupported call error
func procGetsid(int) (int, error) { return 0, windows.ERROR_CALL_NOT_IMPLEMENTED }
