//go:build !windows

package home

import "golang.org/x/sys/unix"

// Reports whether the real user may execute path, the access check os.access makes
func executableByProcess(path string) bool { return unix.Access(path, unix.X_OK) == nil }
