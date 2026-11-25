package proc

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Process table read from the stat file of every numeric entry under /proc
func snapshot() (Table, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var t Table
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// The command name sits in parentheses and may hold spaces, so the fields follow the last one
		s := string(data)
		end := strings.LastIndexByte(s, ')')
		if end < 0 {
			continue
		}
		f := strings.Fields(s[end+1:])
		if len(f) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		t = append(t, Process{PID: pid, PPID: ppid, Zombie: f[0] == "Z"})
	}
	return t, nil
}
