//go:build !windows

package gateway

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// Whether a pid answers signal zero, a permission refusal alive and any other failure unknown
func observedPIDAlive(pid int) (bool, bool) {
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return true, true
	case errors.Is(err, syscall.ESRCH):
		return false, true
	case errors.Is(err, syscall.EPERM):
		return true, true
	}
	return false, false
}

// linux start ticks or darwin lstart of a pid, unknown when it cannot be read
func observedProcessCreation(pid int) (string, bool) {
	switch runtime.GOOS {
	case "linux":
		raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return "", false
		}
		text := string(raw)
		i := strings.LastIndex(text, ")")
		if i < 0 || i+2 > len(text) {
			return "", false
		}
		fields := strings.Fields(text[i+2:])
		if len(fields) < 20 {
			return "", false
		}
		return "linux:" + fields[19], true
	case "darwin":
		out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return "", false
		}
		value := strings.TrimSpace(string(out))
		if value == "" {
			return "", false
		}
		return "darwin:" + value, true
	}
	return "", false
}
