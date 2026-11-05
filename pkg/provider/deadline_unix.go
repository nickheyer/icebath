//go:build !windows

package provider

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
	deadlineKill = syscall.Kill
	procPPIDMap  = readPPIDMap
)

type procParent struct{ pid, ppid int }

type treeProc struct {
	pid   int
	birth float64
}

var errProcessGone = errors.New("no such process")

// Lists every pid with its parent in ascending pid order like the psutil parent map
func readPPIDMap() []procParent {
	table, err := proc.Snapshot()
	if err != nil {
		return nil
	}
	out := make([]procParent, 0, len(table))
	for _, p := range table {
		out = append(out, procParent{p.PID, p.PPID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pid < out[j].pid })
	return out
}

// Walks descendants depth first like psutil children recursive, keeping only those started after the root
func childrenOf(root treeProc) []treeProc {
	reverse := map[int][]int{}
	for _, e := range procPPIDMap() {
		reverse[e.ppid] = append(reverse[e.ppid], e.pid)
	}
	var out []treeProc
	seen := map[int]bool{}
	stack := []int{root.pid}
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		for _, child := range reverse[pid] {
			birth, err := proc.Birth(child)
			if err != nil || root.birth > birth {
				continue
			}
			out = append(out, treeProc{child, birth})
			stack = append(stack, child)
		}
	}
	return out
}

// Reports whether the pid still names the process recorded at snapshot time, zombies included
func (p treeProc) running() bool {
	birth, err := proc.Birth(p.pid)
	return err == nil && birth == p.birth
}

// Signals the recorded process unless its pid was reused, mapping a vanished pid to errProcessGone
func (p treeProc) signal(sig syscall.Signal) error {
	if !p.running() {
		return errProcessGone
	}
	if err := deadlineKill(p.pid, sig); err != nil {
		if err == syscall.ESRCH {
			return errProcessGone
		}
		return err
	}
	return nil
}

// Stops each hard kill target before discovering its children so a fork cannot escape the snapshot
func processTreeSnapshot(pid int, hardKill bool) (descendants []treeProc, stopped []treeProc) {
	rootBirth, err := proc.Birth(pid)
	if err != nil {
		slog.Debug(fmt.Sprintf("kill_process_tree: snapshot incomplete for pid %d: %v", pid, err))
		return nil, nil
	}
	root := treeProc{pid, rootBirth}
	descendants = childrenOf(root)
	if !hardKill {
		return descendants, nil
	}
	incomplete := func(reason string) ([]treeProc, []treeProc) {
		slog.Debug(fmt.Sprintf("kill_process_tree: snapshot incomplete for pid %d: %s", pid, reason))
		return descendants, stopped
	}
	pending := []treeProc{root}
	seen := map[int]bool{os.Getpid(): true}
	known := map[int]bool{}
	for _, d := range descendants {
		known[d.pid] = true
	}
	stopDeadline := deadlineNow().Add(time.Second)
	for len(pending) > 0 {
		for _, p := range pending {
			if seen[p.pid] {
				continue
			}
			seen[p.pid] = true
			if !deadlineNow().Before(stopDeadline) {
				return incomplete("process tree did not stop before snapshot deadline")
			}
			state, err := proc.State(p.pid)
			if err != nil || state == 'Z' || state == 'X' {
				continue
			}
			if state != 'T' {
				err := p.signal(syscall.SIGSTOP)
				if err == errProcessGone {
					continue
				}
				if err != nil {
					return incomplete(err.Error())
				}
				stopped = append(stopped, p)
			}
			for {
				state, err := proc.State(p.pid)
				if err != nil || state == 'T' {
					break
				}
				if !deadlineNow().Before(stopDeadline) {
					return incomplete("process did not stop before snapshot deadline")
				}
				time.Sleep(time.Millisecond)
			}
		}
		for _, c := range childrenOf(root) {
			if !known[c.pid] {
				known[c.pid] = true
				descendants = append(descendants, c)
			}
		}
		pending = pending[:0]
		for _, d := range descendants {
			if !seen[d.pid] {
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
			if err := p.signal(syscall.SIGCONT); err != nil {
				slog.Debug(fmt.Sprintf("kill_process_tree: target already gone or resume refused: %v", err))
			}
		}
	}()
	signalled := false
	for i := len(descendants) - 1; i >= 0; i-- {
		if descendants[i].signal(sig) == nil {
			signalled = true
		}
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		pgid = -1
	}
	if pgid == pid {
		err = deadlineKill(-pgid, sig)
	} else {
		err = deadlineKill(pid, sig)
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
