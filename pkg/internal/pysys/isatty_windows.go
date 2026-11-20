//go:build windows

package pysys

import "golang.org/x/sys/windows"

// Reports a handle the C runtime isatty answers true for, any character device such as a console or NUL
func fdIsatty(fd uintptr) bool {
	kind, err := windows.GetFileType(windows.Handle(fd))
	return err == nil && kind == windows.FILE_TYPE_CHAR
}
