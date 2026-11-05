//go:build darwin

package provider

import (
	"bytes"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// kernel_task holds pid 0 on macOS
const psLowestPID = 0

// Executable path from proc_pidpath through the proc_info syscall
func psExe(pid int) (string, error) {
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

// Parent pid from the kernel process record
func psPPID(pid int) (int, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	if kp.Proc.P_pid != int32(pid) {
		return 0, syscall.ESRCH
	}
	return int(kp.Eproc.Ppid), nil
}

// kill(pid, 0) reachability with pid 0 counted as kernel_task
func psPidExists(pid int) bool {
	if pid < 0 {
		return false
	}
	if pid == 0 {
		return true
	}
	return psKillZero(pid)
}
