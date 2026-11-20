//go:build windows

package backup

import (
	"os"
	"syscall"
)

// Reports whether lstat data carries the reparse point attribute of a junction or other link
func isReparsePoint(info os.FileInfo) bool {
	attrs, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && attrs.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}
