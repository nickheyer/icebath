package dl

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// Adds a directory searched for DLLs and their dependencies by later opens until the returned remove runs
func AddDLLDirectory(dir string) (remove func() error, err error) {
	full, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("add DLL directory %s: %w", dir, err)
	}
	path, err := windows.UTF16PtrFromString(full)
	if err != nil {
		return nil, fmt.Errorf("add DLL directory %s: %w", full, err)
	}
	cookie, err := windows.AddDllDirectory(path)
	if err != nil {
		return nil, fmt.Errorf("add DLL directory %s: %w", full, err)
	}
	return func() error { return windows.RemoveDllDirectory(cookie) }, nil
}
