//go:build windows

package gateway

import "time"

// Response of the runtime control pipe to one request, as the TUI bootstrap reads it on Windows
func TUIBootstrapControlQuery(home string, request []byte, timeout time.Duration) ([]byte, error) {
	return queryRuntimeControl(home, request, timeout)
}
