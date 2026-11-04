//go:build !windows

package provider

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func readProcStat(pid int) (ppid int, state byte, ok bool) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, 0, false
	}
	text := string(raw)
	end := strings.LastIndexByte(text, ')')
	if end < 0 {
		return 0, 0, false
	}
	fields := strings.Fields(text[end+1:])
	if len(fields) < 2 {
		return 0, 0, false
	}
	ppid, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, false
	}
	return ppid, fields[0][0], true
}

func childrenOf(root int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	parents := map[int]int{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if ppid, _, ok := readProcStat(pid); ok {
			parents[pid] = ppid
		}
	}
	var out []int
	queue := []int{root}
	seen := map[int]bool{root: true}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for pid, ppid := range parents {
			if ppid == cur && !seen[pid] {
				seen[pid] = true
				out = append(out, pid)
				queue = append(queue, pid)
			}
		}
	}
	return out
}

func procTreeAlive(pid int) bool {
	_, state, ok := readProcStat(pid)
	return ok && state != 'Z' && state != 'X'
}

// processTreeSnapshot stops each hard kill target before discovering its children so a fork cannot escape
func processTreeSnapshot(pid int, hardKill bool) (descendants []int, stopped []int) {
	descendants = childrenOf(pid)
	if !hardKill {
		return descendants, nil
	}
	pending := []int{pid}
	seen := map[int]bool{os.Getpid(): true}
	known := map[int]bool{}
	for _, d := range descendants {
		known[d] = true
	}
	stopDeadline := deadlineNow().Add(time.Second)
	for len(pending) > 0 {
		for _, p := range pending {
			if seen[p] {
				continue
			}
			seen[p] = true
			if deadlineNow().After(stopDeadline) {
				slog.Debug(fmt.Sprintf("kill_process_tree: snapshot incomplete for pid %d", pid))
				return descendants, stopped
			}
			_, state, ok := readProcStat(p)
			if !ok || state == 'Z' || state == 'X' {
				continue
			}
			if state != 'T' && state != 't' {
				if err := syscall.Kill(p, syscall.SIGSTOP); err == nil {
					stopped = append(stopped, p)
				}
			}
			for {
				_, st, ok := readProcStat(p)
				if !ok || st == 'T' || st == 't' {
					break
				}
				if deadlineNow().After(stopDeadline) {
					slog.Debug(fmt.Sprintf("kill_process_tree: snapshot incomplete for pid %d", pid))
					return descendants, stopped
				}
				time.Sleep(time.Millisecond)
			}
		}
		for _, c := range childrenOf(pid) {
			if !known[c] {
				known[c] = true
				descendants = append(descendants, c)
			}
		}
		pending = pending[:0]
		for _, d := range descendants {
			if !seen[d] {
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
			if err := syscall.Kill(p, syscall.SIGCONT); err != nil {
				slog.Debug(fmt.Sprintf("kill_process_tree: target already gone or resume refused: %v", err))
			}
		}
	}()
	signalled := false
	for i := len(descendants) - 1; i >= 0; i-- {
		child := descendants[i]
		if procTreeAlive(child) && syscall.Kill(child, sig) == nil {
			signalled = true
		}
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		pgid = -1
	}
	if pgid == pid {
		err = syscall.Kill(-pgid, sig)
	} else {
		err = syscall.Kill(pid, sig)
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
