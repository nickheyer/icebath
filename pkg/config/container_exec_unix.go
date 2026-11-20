//go:build !windows

package config

import (
	"os"
	"syscall"
)

// Replaces the process image with the container exec command, only an exec failure returns
func execContainerCommand(argv []string) (int, error) {
	return 1, syscall.Exec(argv[0], argv, os.Environ())
}
