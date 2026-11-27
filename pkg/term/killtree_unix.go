//go:build !windows

package term

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func killPID(pid int) error {
	return syscall.Kill(pid, syscall.SIGKILL)
}

// Live processes of this machine as pid to parent pid, zombies left out, from /proc or else ps; none when neither can be read
func Processes() map[int]int {
	table := map[int]int{}
	if entries, err := os.ReadDir("/proc"); err == nil {
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil {
				continue
			}
			data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
			if err != nil {
				continue
			}
			// The command name sits in parentheses and may hold spaces, so the fields follow the last one
			stat := string(data)
			end := strings.LastIndexByte(stat, ')')
			if end < 0 {
				continue
			}
			fields := strings.Fields(stat[end+1:])
			if len(fields) < 2 || fields[0] == "Z" {
				continue
			}
			if ppid, err := strconv.Atoi(fields[1]); err == nil {
				table[pid] = ppid
			}
		}
		return table
	}
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,stat=").Output()
	if err != nil {
		return table
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || len(fields) >= 3 && strings.HasPrefix(fields[2], "Z") {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 == nil && err2 == nil {
			table[pid] = ppid
		}
	}
	return table
}
