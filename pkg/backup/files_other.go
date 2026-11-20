//go:build !windows

package backup

import "os"

// Reports reparse points, which exist only on Windows
func isReparsePoint(os.FileInfo) bool { return false }
