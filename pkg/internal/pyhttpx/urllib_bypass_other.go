//go:build !darwin && !windows

package pyhttpx

// No operating system bypass list outside macOS and Windows
func systemProxyBypass(string) bool { return false }
