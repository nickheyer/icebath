package proc

import (
	"strings"

	"golang.org/x/sys/unix"
)

// OpenBSD reads every process cwd through the kern.proc_cwd sysctl
const CwdAvailable = true

// Working directory of pid from the KERN_PROC_CWD sysctl
func Cwd(pid int) (string, error) {
	raw, err := unix.SysctlRaw("kern.proc_cwd", pid)
	if err != nil {
		return "", err
	}
	path, _, _ := strings.Cut(string(raw), "\x00")
	return path, nil
}
