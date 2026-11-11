//go:build !windows

package app

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Creation flags that detach a child, zero off Windows
func windowsDetachFlags() uint32 { return 0 }

// Detach flags without the job breakaway, zero off Windows
func windowsDetachFlagsWithoutBreakaway() uint32 { return 0 }

// Process attributes carrying Windows creation flags, none off Windows
func creationFlagsAttr(flags uint32) *syscall.SysProcAttr { return nil }

// Process attributes that let a child outlive this process, a new session on POSIX
func detachPopenAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// Whether this process may write the path, the os.access W_OK check
func accessWritable(p string) bool { return unix.Access(p, unix.W_OK) == nil }

// Owner uid of a stat result
func fileOwnerUID(st os.FileInfo) int {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return int(sys.Uid)
	}
	return -1
}
