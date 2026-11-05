//go:build !windows

package pm

import "golang.org/x/sys/unix"

// Reports whether this process may execute a file, as os access with X_OK answers
func canExecute(path string) bool { return unix.Access(path, unix.X_OK) == nil }
