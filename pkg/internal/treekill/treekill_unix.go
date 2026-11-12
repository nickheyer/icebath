//go:build !windows

package treekill

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Signal sender and process table reader, swapped by tests to refuse signals or schedule a fork
var (
	Signal    = syscall.Kill
	ParentMap = ReadParentMap
)

// Parent is one process table row naming a pid and its parent pid
type Parent struct{ PID, PPID int }

// Proc is a process identity recorded as its pid and start time so a reused pid is never signalled
type Proc struct {
	PID   int
	Birth float64
}

// ErrProcessGone reports a recorded process that exited or whose pid now names another process
var ErrProcessGone = errors.New("no such process")

// ReadParentMap lists every pid with its parent in ascending pid order like the psutil parent map
func ReadParentMap() []Parent {
	table, err := proc.Snapshot()
	if err != nil {
		return nil
	}
	out := make([]Parent, 0, len(table))
	for _, p := range table {
		out = append(out, Parent{p.PID, p.PPID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// Walks descendants depth first like psutil children recursive, keeping only those started after the root
func childrenOf(root Proc) []Proc {
	reverse := map[int][]int{}
	for _, e := range ParentMap() {
		reverse[e.PPID] = append(reverse[e.PPID], e.PID)
	}
	var out []Proc
	seen := map[int]bool{}
	stack := []int{root.PID}
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		for _, child := range reverse[pid] {
			birth, err := proc.Birth(child)
			if err != nil || root.Birth > birth {
				continue
			}
			out = append(out, Proc{child, birth})
			stack = append(stack, child)
		}
	}
	return out
}

// Running reports whether the pid still names the process recorded at snapshot time, zombies included
func (p Proc) Running() bool {
	birth, err := proc.Birth(p.PID)
	return err == nil && birth == p.Birth
}

// Signal sends sig to the recorded process unless its pid was reused, a vanished pid giving ErrProcessGone
func (p Proc) Signal(sig syscall.Signal) error {
	if !p.Running() {
		return ErrProcessGone
	}
	if err := Signal(p.PID, sig); err != nil {
		if err == syscall.ESRCH {
			return ErrProcessGone
		}
		return err
	}
	return nil
}

// Stops each hard kill target before discovering its children so a fork cannot escape the snapshot
func processTreeSnapshot(pid int, hardKill bool) (descendants []Proc, stopped []Proc) {
	rootBirth, err := proc.Birth(pid)
	if err != nil {
		slog.Debug(fmt.Sprintf("kill_process_tree: snapshot incomplete for pid %d: %v", pid, err))
		return nil, nil
	}
	root := Proc{pid, rootBirth}
	descendants = childrenOf(root)
	if !hardKill {
		return descendants, nil
	}
	incomplete := func(reason string) ([]Proc, []Proc) {
		slog.Debug(fmt.Sprintf("kill_process_tree: snapshot incomplete for pid %d: %s", pid, reason))
		return descendants, stopped
	}
	pending := []Proc{root}
	seen := map[int]bool{os.Getpid(): true}
	known := map[int]bool{}
	for _, d := range descendants {
		known[d.PID] = true
	}
	stopDeadline := time.Now().Add(time.Second)
	for len(pending) > 0 {
		for _, p := range pending {
			if seen[p.PID] {
				continue
			}
			seen[p.PID] = true
			if !time.Now().Before(stopDeadline) {
				return incomplete("process tree did not stop before snapshot deadline")
			}
			state, err := proc.State(p.PID)
			if err != nil || state == 'Z' || state == 'X' {
				continue
			}
			if state != 'T' {
				err := p.Signal(syscall.SIGSTOP)
				if err == ErrProcessGone {
					continue
				}
				if err != nil {
					return incomplete(err.Error())
				}
				stopped = append(stopped, p)
			}
			for {
				state, err := proc.State(p.PID)
				if err != nil || state == 'T' {
					break
				}
				if !time.Now().Before(stopDeadline) {
					return incomplete("process did not stop before snapshot deadline")
				}
				time.Sleep(time.Millisecond)
			}
		}
		for _, c := range childrenOf(root) {
			if !known[c.PID] {
				known[c.PID] = true
				descendants = append(descendants, c)
			}
		}
		pending = pending[:0]
		for _, d := range descendants {
			if !seen[d.PID] {
				pending = append(pending, d)
			}
		}
	}
	return descendants, stopped
}

// KillProcessTree signals a process and every descendant, children first then the group, true when anything was signalled
func KillProcessTree(pid int, sig syscall.Signal) bool {
	if sig == 0 {
		sig = syscall.SIGKILL
	}
	descendants, stopped := processTreeSnapshot(pid, sig == syscall.SIGKILL)
	defer func() {
		for _, p := range stopped {
			if err := p.Signal(syscall.SIGCONT); err != nil {
				slog.Debug(fmt.Sprintf("kill_process_tree: target already gone or resume refused: %v", err))
			}
		}
	}()
	signalled := false
	for i := len(descendants) - 1; i >= 0; i-- {
		if descendants[i].Signal(sig) == nil {
			signalled = true
		}
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		pgid = -1
	}
	if pgid == pid {
		err = Signal(-pgid, sig)
	} else {
		err = Signal(pid, sig)
	}
	switch {
	case err == nil:
		signalled = true
	case err == syscall.ESRCH:
	default:
		slog.Debug(fmt.Sprintf("kill_process_tree: signal failed for pid %d: %v", pid, err))
	}
	return signalled
}
