//go:build windows

package app

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// Creation flags that detach a child from the console, process group and job object
func windowsDetachFlags() uint32 {
	return windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW | windows.CREATE_BREAKAWAY_FROM_JOB
}

// Detach flags without the job breakaway a restrictive job object refuses
func windowsDetachFlagsWithoutBreakaway() uint32 {
	return windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW
}

// Process attributes carrying Windows creation flags
func creationFlagsAttr(flags uint32) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: flags}
}

// Process attributes that let a child outlive this process, the detach flags on Windows
func detachPopenAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windowsDetachFlags()}
}

// Whether this process may write the path, the os.access W_OK check that reads the read only attribute
func accessWritable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && (st.IsDir() || st.Mode().Perm()&0o200 != 0)
}

// Owner uid of a stat result, zero on Windows where os.stat reports no owner
func fileOwnerUID(st os.FileInfo) int { return 0 }
