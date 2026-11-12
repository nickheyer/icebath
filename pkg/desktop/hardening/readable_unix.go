//go:build !windows

package hardening

import "golang.org/x/sys/unix"

// True when this process may read path as fs.access with R_OK answers
func readable(path string) bool { return unix.Access(path, unix.R_OK) == nil }
