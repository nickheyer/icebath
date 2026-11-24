//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !windows

package fsutil

// Copies nothing on a platform with no call that changes a symlink itself
func CopyLinkMetadata(_, _ string) error { return nil }
