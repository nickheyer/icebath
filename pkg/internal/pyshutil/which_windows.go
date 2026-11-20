//go:build windows

package pyshutil

import "os"

// Existence of the file, the only check os.access X_OK makes on Windows
func accessExecutable(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
