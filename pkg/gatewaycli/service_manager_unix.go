//go:build unix

package gatewaycli

import (
	"os"
	"syscall"
)

// Creates a named pipe with the mode under the process umask
func mkfifo(path string, mode uint32) error { return syscall.Mkfifo(path, mode) }

// Changes the owner of a path without following a final link
func lchown(path string, uid, gid int) error { return os.Lchown(path, uid, gid) }
