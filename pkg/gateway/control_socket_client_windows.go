//go:build windows

package gateway

import "time"

// Windows has no private runtime directory for the socket fallback
func privateRuntimeDir() string { return "" }

// One request and response over the native control pipe, abandoned at the deadline
func controlQuery(home string, request []byte, timeout time.Duration) ([]byte, error) {
	return queryWindowsPipe(home, request, timeout)
}
