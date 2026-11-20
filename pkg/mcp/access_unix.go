//go:build !windows

package mcp

import "golang.org/x/sys/unix"

// Reports whether the process may execute the file the way os.access X_OK answers
func accessX(p string) bool { return unix.Access(p, unix.X_OK) == nil }
