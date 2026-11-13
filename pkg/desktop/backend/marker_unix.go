//go:build !windows

package backend

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func hideWindow(*exec.Cmd) {}

// IsPidAlive is true when signal 0 reaches pid or is refused for permission
func IsPidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

var digits = regexp.MustCompile(`^\d+$`)

// ProcessStartMarker returns a stable start identity of pid, the /proc start tick on Linux and ps lstart elsewhere
func ProcessStartMarker(pid int) (string, error) {
	if !IsPidAlive(pid) {
		return "", &NoProcessError{Pid: pid}
	}
	if runtime.GOOS == "linux" {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return "", err
		}
		stat := string(raw)
		fields := strings.Fields(strings.TrimSpace(stat[strings.LastIndex(stat, ")")+1:]))
		if len(fields) < 20 || !digits.MatchString(fields[19]) {
			return "", fmt.Errorf("Invalid /proc start marker for PID %d", pid)
		}
		return "linux:" + fields[19], nil
	}
	started, err := ExecText("ps", []string{"-p", strconv.Itoa(pid), "-o", "lstart="}, 3*time.Second)
	if err != nil {
		return "", err
	}
	if started == "" {
		return "", fmt.Errorf("Missing process start marker for PID %d", pid)
	}
	return "ps:" + started, nil
}

// KillGroup signals a process group by negative id with a named signal
func KillGroup(pgid int, signal string) error {
	sig := map[string]syscall.Signal{"SIGTERM": syscall.SIGTERM, "SIGKILL": syscall.SIGKILL}[signal]
	return syscall.Kill(pgid, sig)
}

// ForceKillProcessTree is the Windows tree kill and never runs on POSIX
func ForceKillProcessTree(int) { panic("process tree kill runs only on Windows") }

// Windows tree kill, nothing to end on POSIX
func TaskkillTree(int) error { return nil }
