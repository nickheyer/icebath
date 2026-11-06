//go:build unix

package gatewaycli

import "syscall"

// Creates a named pipe with the mode under the process umask
func mkfifo(path string, mode uint32) error { return syscall.Mkfifo(path, mode) }
