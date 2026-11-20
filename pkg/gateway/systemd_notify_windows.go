//go:build windows

package gateway

// Reports failure since the notify socket is never set on windows
func systemdNotifySend(address, message string) bool {
	return false
}
