//go:build windows

package gateway

// Windows delivers no SIGHUP so nothing changes
func ignoreHangupSignal() {}
