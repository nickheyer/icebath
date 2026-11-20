//go:build !windows

package pyshutil

import "syscall"

// os.rmdir of an empty directory reported as a path error
func syscallRmdir(path string) error {
	if err := syscall.Rmdir(path); err != nil {
		return &osPathError{Op: "rmdir", Path: path, Err: err}
	}
	return nil
}
