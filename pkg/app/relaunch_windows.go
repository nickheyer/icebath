//go:build windows

package app

import (
	"errors"
	"os"
)

// Whether the process may execute a path, any existing path as the Windows access check answers
func accessExecutable(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Windows has no exec that replaces the process image, relaunch spawns there instead
func execvp(file string, argv []string) error {
	return errors.New("exec that replaces the process is unavailable on Windows: " + file)
}
