//go:build !windows

package state

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// psutil.pid_exists, signal zero existence with the Linux thread id filter through the status Tgid line
func psutilPIDExists(pid int) bool {
	if pid < 0 {
		return false
	}
	if pid > 0 {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return false
		}
	}
	if runtime.GOOS != "linux" {
		return true
	}
	if f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid)); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if rest, ok := strings.CutPrefix(sc.Text(), "Tgid:"); ok {
				if tgid, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
					return tgid == pid
				}
				break
			}
		}
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name() == strconv.Itoa(pid) {
			return true
		}
	}
	return false
}
