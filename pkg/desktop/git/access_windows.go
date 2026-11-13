//go:build windows

package git

import "os"

// True when the entry exists, which is all fs.access with R_OK checks on Windows
func readable(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
