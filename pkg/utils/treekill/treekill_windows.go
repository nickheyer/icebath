//go:build windows

package treekill

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/utils/proc"
)

// Exit code every process ended by a tree kill reports, the code TerminateProcess callers use for a kill
const killedExitCode = 1

// KillProcessTree ends a process and its descendants, the whole job when pid leads a tracked tree, the signal is ignored
func KillProcessTree(pid int, sig syscall.Signal) bool {
	if job := Tracked(pid); job != nil {
		if n, err := job.Active(); err == nil && n == 0 {
			return false
		}
		if err := job.Terminate(killedExitCode); err != nil {
			pkgLog().Debug(fmt.Sprintf("kill_process_tree: job terminate failed for pid %d: %v", pid, err))
			return false
		}
		return true
	}
	return killSnapshotTree(pid)
}

// Ends pid and every descendant born after its parent, sweeping again for children started during a pass
func killSnapshotTree(pid int) bool {
	rootBirth, err := processBirth(pid)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("kill_process_tree: snapshot incomplete for pid %d: %v", pid, err))
		return false
	}
	signalled := false
	for pass := 0; pass < 3; pass++ {
		targets := descendants(pid, rootBirth)
		if birth, err := processBirth(pid); err == nil && birth == rootBirth {
			targets = append([]int{pid}, targets...)
		} else if err == nil {
			return signalled
		}
		if len(targets) == 0 {
			return signalled
		}
		for _, target := range targets {
			if err := terminatePid(target, killedExitCode); err == nil {
				signalled = true
			} else {
				pkgLog().Debug(fmt.Sprintf("kill_process_tree: terminate failed for pid %d: %v", target, err))
			}
		}
	}
	return signalled
}

// Live descendants of root parents first, each born no earlier than its parent so a reused parent pid adds nothing
func descendants(root int, rootBirth float64) []int {
	table, err := proc.Snapshot()
	if err != nil {
		return nil
	}
	kids := map[int][]int{}
	for _, p := range table {
		if p.PID != p.PPID {
			kids[p.PPID] = append(kids[p.PPID], p.PID)
		}
	}
	self := os.Getpid()
	births := map[int]float64{root: rootBirth}
	seen := map[int]bool{root: true}
	var out []int
	queue := []int{root}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, child := range kids[parent] {
			if seen[child] || child == self {
				continue
			}
			birth, err := processBirth(child)
			if err != nil || birth < births[parent] {
				continue
			}
			seen[child] = true
			births[child] = birth
			out = append(out, child)
			queue = append(queue, child)
		}
	}
	return out
}

// Creation time of a live process, an error once it exited
func processBirth(pid int) (float64, error) {
	if !processRunning(pid) {
		return 0, ErrProcessGone
	}
	return proc.Birth(pid)
}

// Reports whether pid names a process that has not exited
func processRunning(pid int) bool { return pid > 0 && proc.Exists(pid) }

// Ends one process with exit code code
func terminatePid(pid int, code uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, code)
}
