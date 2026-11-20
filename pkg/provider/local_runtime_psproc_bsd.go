//go:build unix && !linux && !darwin

package provider

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// The swapper holds pid 0 on the BSDs
const psLowestPID = 0

// Parent pid from ps
func psPPID(pid int) (int, error) {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, errors.New("ps reported no parent")
	}
	return strconv.Atoi(fields[0])
}

// kill(pid, 0) reachability with pid 0 counted as the swapper
func psPidExists(pid int) bool {
	if pid < 0 {
		return false
	}
	if pid == 0 {
		return true
	}
	return psKillZero(pid)
}
