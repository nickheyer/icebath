//go:build !windows

package fsutil

// Junctions exist only on Windows
func isMountPoint(string) bool { return false }
