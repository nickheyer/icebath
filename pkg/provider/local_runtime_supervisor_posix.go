//go:build !windows

package provider

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// Clears SO_REUSEADDR so a port probe fails like a plain Python bind on a lingering port
func lrBindControl(network, address string, c syscall.RawConn) error {
	var opErr error
	err := c.Control(func(fd uintptr) {
		opErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 0)
	})
	if err != nil {
		return err
	}
	return opErr
}

// Every live pid from /proc, or from ps where there is no procfs
func lrAllPids() []int {
	var out []int
	if ents, err := os.ReadDir("/proc"); err == nil {
		for _, e := range ents {
			if pid, err := strconv.Atoi(e.Name()); err == nil {
				out = append(out, pid)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	raw, err := exec.Command("ps", "-axo", "pid=").Output()
	if err != nil {
		return nil
	}
	for _, field := range strings.Fields(string(raw)) {
		if pid, err := strconv.Atoi(field); err == nil {
			out = append(out, pid)
		}
	}
	return out
}

func lrTryLockBootFile(f *os.File) bool {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
}

func lrUnlockBootFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
