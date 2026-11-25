//go:build !windows && !linux

package proc

import (
	"os/exec"
	"strconv"
	"strings"
)

// Process table read from ps, which every BSD derived system ships
func snapshot() (Table, error) {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,stat=").Output()
	if err != nil {
		return nil, err
	}
	var t Table
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		t = append(t, Process{PID: pid, PPID: ppid, Zombie: strings.HasPrefix(f[2], "Z")})
	}
	return t, nil
}
