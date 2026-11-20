//go:build !windows

package backup

import "os"

// Lock files need no preparation where flock locks the whole file
func prepareLockFile(string, *os.File) error { return nil }
