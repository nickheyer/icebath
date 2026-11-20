//go:build darwin

package provider

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// kernel_task holds pid 0 on macOS
const psLowestPID = 0

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
