//go:build !windows

package git

import "golang.org/x/sys/unix"

// True when the real user may read the path, as fs.access with R_OK answers
func readable(path string) bool { return unix.Access(path, unix.R_OK) == nil }
