//go:build windows

package dlutil

import (
	"io/fs"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// Adds a directory to the DLL search path of later loads until the returned function runs
func AddDLLDirectory(dir string) (func(), error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, &fs.PathError{Op: "add dll directory", Path: dir, Err: err}
	}
	p, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return nil, &fs.PathError{Op: "add dll directory", Path: dir, Err: err}
	}
	cookie, err := windows.AddDllDirectory(p)
	if err != nil {
		return nil, &fs.PathError{Op: "add dll directory", Path: dir, Err: err}
	}
	return func() { _ = windows.RemoveDllDirectory(cookie) }, nil
}
