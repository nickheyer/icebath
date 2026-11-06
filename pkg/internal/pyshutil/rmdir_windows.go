package pyshutil

import "syscall"

// os.rmdir of an empty directory reported as a path error
func syscallRmdir(path string) error {
	p, err := syscall.UTF16PtrFromString(path)
	if err == nil {
		err = syscall.RemoveDirectory(p)
	}
	if err != nil {
		return &osPathError{Op: "rmdir", Path: path, Err: err}
	}
	return nil
}
