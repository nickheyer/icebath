//go:build !windows

package app

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// Whether the process may execute a path, the X_OK access check
func accessExecutable(p string) bool { return unix.Access(p, unix.X_OK) == nil }

// Replaces the process image with a program found on PATH when its name has no slash, env its environment (this process's when nil)
func execvp(file string, argv, env []string) error {
	path, err := exec.LookPath(file)
	if err != nil {
		return err
	}
	if env == nil {
		env = os.Environ()
	}
	return syscall.Exec(path, argv, env)
}
