//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !windows

package pyshutil

// Copies nothing on a platform with no call that changes a symlink itself
func CopyLinkStat(_, _ string) error { return nil }
