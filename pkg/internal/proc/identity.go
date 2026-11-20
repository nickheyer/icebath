package proc

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
)

// Error of a probe whose pid does not exist
var ErrNoSuchProcess = errors.New("no such process")

// Every process the OS lists at one instant
func Snapshot() (Table, error) { return snapshot() }

var lowest struct {
	mu  sync.Mutex
	pid int
	ok  bool
}

// Lowest pid the OS lists, cached after the first successful listing like psutil
func lowestPID() (int, error) {
	lowest.mu.Lock()
	defer lowest.mu.Unlock()
	if lowest.ok {
		return lowest.pid, nil
	}
	pids, err := listPIDs()
	if err != nil {
		return 0, err
	}
	if len(pids) == 0 {
		return 0, errors.New("the process listing is empty")
	}
	low := pids[0]
	for _, pid := range pids[1:] {
		low = min(low, pid)
	}
	lowest.pid, lowest.ok = low, true
	return low, nil
}

// Ancestors of pid nearest first like psutil, ending at the lowest pid, a vanished parent or a reused pid
func Parents(pid int) ([]int, error) {
	low, err := lowestPID()
	if err != nil {
		return nil, err
	}
	out := []int{}
	seen := map[int]bool{pid: true}
	for cur := pid; cur != low; {
		ppid, ok := PPID(cur)
		if !ok {
			return nil, fmt.Errorf("parent pid of %d is unreadable", cur)
		}
		created, err := Birth(cur)
		if err != nil {
			return nil, err
		}
		parentCreated, err := Birth(ppid)
		if errors.Is(err, ErrNoSuchProcess) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if parentCreated > created || seen[ppid] {
			return out, nil
		}
		seen[ppid] = true
		out = append(out, ppid)
		cur = ppid
	}
	return out, nil
}

// Splits a NUL separated environment block like psutil, stopping at a double NUL and upper casing keys on Windows
func parseEnvironBlock(data string) map[string]string {
	out := map[string]string{}
	pos := 0
	for {
		next := strings.IndexByte(data[pos:], 0)
		if next <= 0 {
			return out
		}
		next += pos
		if eq := strings.IndexByte(data[pos:next], '='); eq > 0 {
			key := data[pos : pos+eq]
			if runtime.GOOS == "windows" {
				key = strings.ToUpper(key)
			}
			out[key] = data[pos+eq+1 : next]
		}
		pos = next + 1
	}
}
