//go:build linux

package provider

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Linux pids start at 1, the init process has no parent
const psLowestPID = 1

// Parent pid from the stat fields after the command name
func psPPID(pid int) (int, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return 0, errors.New("malformed /proc stat")
	}
	fields := strings.Fields(string(data[i+1:]))
	if len(fields) < 2 {
		return 0, errors.New("malformed /proc stat")
	}
	return strconv.Atoi(fields[1])
}

// Live process that is a thread group leader, the process table decides when status is unreadable
func psPidExists(pid int) bool {
	if pid <= 0 || !psKillZero(pid) {
		return false
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if rest, ok := strings.CutPrefix(line, "Tgid:"); ok {
				if tgid, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
					return tgid == pid
				}
				break
			}
		}
	}
	_, err = os.Stat(filepath.Join("/proc", strconv.Itoa(pid)))
	return err == nil
}
