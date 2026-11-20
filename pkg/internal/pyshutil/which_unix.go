//go:build !windows

package pyshutil

import "golang.org/x/sys/unix"

// Execute permission for this process as os.access with X_OK reports it
func accessExecutable(p string) bool { return unix.Access(p, unix.X_OK) == nil }
