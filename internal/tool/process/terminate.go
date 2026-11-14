package process

import (
	"context"
	"log/slog"
	"math"
	"slices"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/proc"
)

const (
	defaultTermGrace = 2 * time.Second
	killSettle       = time.Second
	pollStep         = 50 * time.Millisecond
)

type hostFate int

const (
	fateRunning hostFate = iota
	fateGone
	fateReused
)

func startFingerprint(pid int) *int64 {
	if pid == 0 {
		return nil
	}
	v, err := proc.Fingerprint(pid)
	if err != nil {
		return nil
	}
	return &v
}

func hostPIDAlive(pid int) bool { return pid != 0 && proc.Alive(pid) }

func hostPIDIsOurs(pid int, expected *int64) bool {
	if !hostPIDAlive(pid) {
		return false
	}
	if expected == nil {
		return true
	}
	current := startFingerprint(pid)
	return current != nil && *current == *expected
}

func detachedHostFate(pid int, expected *int64) hostFate {
	if !hostPIDAlive(pid) {
		return fateGone
	}
	if expected == nil {
		return fateRunning
	}
	current := startFingerprint(pid)
	if current == nil || *current == *expected {
		return fateRunning
	}
	return fateReused
}

func TermGrace(ctx context.Context, cfg *config.Store) time.Duration {
	c, err := cfg.LoadReadonly(ctx)
	if err != nil {
		return defaultTermGrace
	}
	seconds := c.Terminal.DaemonTermGraceSeconds
	if math.IsNaN(seconds) || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

func Terminate(ctx context.Context, pid int, expectedStart *int64, grace time.Duration) {
	if expectedStart != nil && !hostPIDIsOurs(pid, expectedStart) {
		slog.Warn("Refusing to terminate host pid: start-time mismatch, the PID was recycled onto an unrelated process", "pid", pid)
		return
	}
	terminateTree(ctx, pid, grace)
}

func liveDescendants(pid int) []int {
	root, err := proc.Identify(pid)
	if err != nil {
		slog.Debug("Could not list descendants of pid", "pid", pid, "err", err)
		return nil
	}
	table, err := proc.ReadTable()
	if err != nil {
		slog.Debug("Could not list descendants of pid", "pid", pid, "err", err)
		return nil
	}
	var out []int
	for _, d := range table.Descendants(root) {
		if d.Alive() {
			out = append(out, d.PID)
		}
	}
	return out
}

func (r *Registry) postKillSurvivors(s *Session) []int {
	deadline := time.Now().Add(killSettle)
	for {
		survivors := r.probeSurvivors(s)
		if len(survivors) == 0 || !time.Now().Before(deadline) {
			return survivors
		}
		time.Sleep(pollStep)
	}
}

func (r *Registry) probeSurvivors(s *Session) []int {
	s.mu.Lock()
	pid, scope, expected := s.pid, s.pidScope, s.hostStart
	s.mu.Unlock()
	var survivors []int
	if s.process != nil {
		if _, exited := s.process.Exited(); !exited {
			root := s.process.PID()
			if root == 0 {
				root = pid
			}
			survivors = append(survivors, root)
		}
	}
	if s.pty != nil && s.pty.alive() {
		survivors = append(survivors, pid)
	}
	if scope == scopeHost && pid != 0 && detachedHostFate(pid, expected) == fateRunning {
		if !slices.Contains(survivors, pid) {
			survivors = append(survivors, pid)
		}
		for _, d := range liveDescendants(pid) {
			if !slices.Contains(survivors, d) {
				survivors = append(survivors, d)
			}
		}
	}
	return slices.DeleteFunc(survivors, func(p int) bool { return p == 0 })
}
