//go:build !windows

package tuiserver

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Pid of a live descendant of this process named name whose arguments carry marker, zero when none runs yet
func descendantRunning(t *testing.T, name, marker string) int {
	t.Helper()
	out, err := exec.Command("ps", "-A", "-o", "pid=", "-o", "ppid=", "-o", "stat=", "-o", "comm=", "-o", "args=").Output()
	if err != nil {
		t.Fatal(err)
	}
	parents := map[int]int{}
	var matches []int
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		pid, _ := strconv.Atoi(fields[0])
		ppid, _ := strconv.Atoi(fields[1])
		parents[pid] = ppid
		if !strings.HasPrefix(fields[2], "Z") && fields[3] == name && strings.Contains(strings.Join(fields[4:], " "), marker) {
			matches = append(matches, pid)
		}
	}
	self := os.Getpid()
	for _, pid := range matches {
		for p, hops := parents[pid], 0; p > 1 && hops < 64; p, hops = parents[p], hops+1 {
			if p == self {
				return pid
			}
		}
	}
	return 0
}

// Waits for the process to end, a zombie counting as ended, false when it outlives the timeout
func waitProcessGone(pid int, timeout time.Duration) bool {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if err != nil || strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			return true
		}
	}
	return false
}

// Waits up to twenty seconds for the foreground sleep carrying the marker to start
func awaitForegroundSleep(t *testing.T, marker string) int {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if pid := descendantRunning(t, "sleep", marker); pid != 0 {
			return pid
		}
	}
	t.Fatal("test setup: foreground sleep never started")
	return 0
}

// Agent whose interrupt raises the interrupt bit of the tool worker of its turn
type midToolAgent struct{ worker tools.WorkerID }

func (a *midToolAgent) Interrupt(message string) { tools.SetInterrupt(a.worker, true, "") }
