//go:build !windows && !linux

package proc

import (
	"os/exec"
	"strconv"
	"strings"
)

// Account owning pid through the real uid ps reports
func Username(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "ruid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", ErrNoSuchProcess
	}
	uid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return "", err
	}
	return uidName(uid), nil
}
