//go:build unix && !linux && !darwin

package proc

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

	"golang.org/x/sys/unix"
)

// Executable path of pid from procfs, the pathname sysctl, or the argv lookup OpenBSD leaves
func Exe(pid int) (string, error) {
	if target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe")); err == nil {
		return target, nil
	}
	if runtime.GOOS != "openbsd" {
		raw, err := unix.SysctlRaw("kern.proc.pathname", pid)
		if err != nil {
			return "", err
		}
		if i := bytes.IndexByte(raw, 0); i >= 0 {
			raw = raw[:i]
		}
		return string(raw), nil
	}
	argv, err := Cmdline(pid)
	if err != nil {
		return "", err
	}
	if len(argv) == 0 {
		return "", nil
	}
	if filepath.IsAbs(argv[0]) {
		return argv[0], nil
	}
	if found, err := exec.LookPath(argv[0]); err == nil {
		return found, nil
	}
	return "", nil
}
