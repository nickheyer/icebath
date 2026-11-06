package state

import (
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

type reapCandidate struct {
	pid     int
	created float64
}

// Signals the process unless its pid was recycled since the candidate was proven, psutil terminate and kill
func (c reapCandidate) signal(sig syscall.Signal) error {
	if created, err := proc.Birth(c.pid); err != nil || created != c.created {
		return syscall.ESRCH
	}
	return syscall.Kill(c.pid, sig)
}

// Candidates still running after timeout, polled with the psutil wait backoff
func waitProcs(candidates []reapCandidate, timeout float64) []reapCandidate {
	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	interval := 100 * time.Microsecond
	for {
		var alive []reapCandidate
		for _, c := range candidates {
			if err := syscall.Kill(c.pid, 0); err == nil || err == syscall.EPERM {
				alive = append(alive, c)
			}
		}
		if len(alive) == 0 || !time.Now().Before(deadline) {
			return alive
		}
		time.Sleep(min(interval, time.Until(deadline)))
		interval = min(interval*2, 40*time.Millisecond)
	}
}

// Terminates old orphaned Desktop port zero backends with no established client, failing closed on any doubt
func reapInactiveOrphanDesktopHolders(dbPath string, holders []Holder, minAgeSeconds float64) ([]int, error) {
	pids, err := concreteStateDBHolderPIDs(dbPath, holders)
	if err != nil {
		return nil, err
	}
	now := float64(time.Now().UnixNano()) / 1e9
	var candidates []reapCandidate
	for _, pid := range pids {
		statuses, err := proc.InetConnectionStatuses(pid)
		if err != nil {
			continue
		}
		ppid, ok := proc.PPID(pid)
		if !ok {
			continue
		}
		created, err := proc.Birth(pid)
		if err != nil {
			continue
		}
		argv, err := proc.Cmdline(pid)
		if err != nil {
			continue
		}
		if !isInactiveOrphanDesktopHolder(ppid, now-created, minAgeSeconds, isEphemeralPortZeroBackend(argv), statuses) {
			continue
		}
		candidates = append(candidates, reapCandidate{pid, created})
	}
	var signalled []int
	for _, c := range candidates {
		if c.signal(syscall.SIGTERM) == nil {
			signalled = append(signalled, c.pid)
		}
	}
	if len(signalled) == 0 {
		return nil, nil
	}
	alive := waitProcs(candidates, 1.5)
	for _, c := range alive {
		_ = c.signal(syscall.SIGKILL)
	}
	if len(alive) > 0 {
		waitProcs(alive, 1.5)
	}
	return signalled, nil
}
