//go:build darwin

package proc

import (
	"bytes"
	"syscall"
	"unsafe"
)

// Executable path of pid from proc_pidpath
func Exe(pid int) (string, error) {
	const procInfoCallPIDInfo, procPIDPathInfo, maxSize = 2, 11, 4096
	buf := make([]byte, maxSize)
	n, _, errno := syscall.Syscall6(syscall.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(pid), procPIDPathInfo, 0,
		uintptr(unsafe.Pointer(&buf[0])), maxSize)
	if errno != 0 {
		return "", errno
	}
	if i := bytes.IndexByte(buf[:n], 0); i >= 0 {
		n = uintptr(i)
	}
	return string(buf[:n]), nil
}
