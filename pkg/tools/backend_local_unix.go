//go:build !windows

package tools

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Hard link count of a stat result
func fileLinkCount(st os.FileInfo) uint64 {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint64(sys.Nlink)
	}
	return 1
}

// Reports whether this process may enter the directory
func dirSearchable(p string) bool {
	return unix.Access(p, unix.X_OK) == nil
}

// Reports whether this process may write and enter the directory
func dirWritable(p string) bool {
	return unix.Access(p, unix.W_OK|unix.X_OK) == nil
}

// Numeric uid of this process
func currentUID() string {
	return strconv.Itoa(os.Getuid())
}

// Starts the child as the leader of a new session and process group
func startNewSession(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// Process group of a pid
func processGroupOf(pid int) (int, error) {
	return syscall.Getpgid(pid)
}

// Start time fingerprint of a process, /proc ticks on Linux else ps lstart in centiseconds
func processStartTime(pid int) (int64, bool) {
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		s := string(data)
		if end := strings.LastIndexByte(s, ')'); end >= 0 {
			fields := strings.Fields(s[end+1:])
			if len(fields) > 19 {
				if ticks, err := strconv.ParseInt(fields[19], 10, 64); err == nil {
					return ticks, true
				}
			}
		}
		return 0, false
	}
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, false
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return 0, false
	}
	for _, layout := range []string{"Mon Jan _2 15:04:05 2006", "Mon _2 Jan 15:04:05 2006", "Mon Jan _2 15:04:05 MST 2006"} {
		if t, err := time.ParseInLocation(layout, text, time.Local); err == nil {
			return t.UnixNano() / 1e7, true
		}
	}
	return 0, false
}

// Start time fingerprint of a live process
func ProcessStartTime(pid int) (int64, bool) { return processStartTime(pid) }

// Live process table as pid and parent pairs from /proc or ps
func localProcessTable() map[int]int {
	table := map[int]int{}
	if ents, err := os.ReadDir("/proc"); err == nil {
		for _, e := range ents {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
			if err != nil {
				continue
			}
			s := string(data)
			end := strings.LastIndexByte(s, ')')
			if end < 0 {
				continue
			}
			f := strings.Fields(s[end+1:])
			if len(f) < 2 || f[0] == "Z" {
				continue
			}
			if ppid, err := strconv.Atoi(f[1]); err == nil {
				table[pid] = ppid
			}
		}
		return table
	}
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,stat=").Output()
	if err != nil {
		return table
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if len(f) >= 3 && strings.HasPrefix(f[2], "Z") {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil {
			table[pid] = ppid
		}
	}
	return table
}

// Every live descendant of pid, parents before their children
func localDescendants(pid int) []int {
	table := localProcessTable()
	kids := map[int][]int{}
	for p, pp := range table {
		kids[pp] = append(kids[pp], p)
	}
	seen := map[int]bool{pid: true}
	var out []int
	queue := []int{pid}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, k := range kids[id] {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
				queue = append(queue, k)
			}
		}
	}
	return out
}

// Snapshot of descendant pids with their start times so a recycled pid is never signalled
type descendantSnapshot struct {
	pid   int
	start int64
	known bool
}

// Descendants of the wrapper with identity fingerprints
func snapshotDescendants(pid int) []descendantSnapshot {
	var out []descendantSnapshot
	for _, d := range localDescendants(pid) {
		start, ok := processStartTime(d)
		out = append(out, descendantSnapshot{pid: d, start: start, known: ok})
	}
	return out
}

// Reports whether the snapshotted descendant is still the same process
func (d descendantSnapshot) running() bool {
	if err := syscall.Kill(d.pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	if !d.known {
		return true
	}
	current, ok := processStartTime(d.pid)
	return !ok || startTimeFingerprintsMatch(d.start, current)
}

// Waits until the process group is gone, polling the wrapper so a dead leader is reaped
func waitForGroupExit(p *ExecProcess, pgid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		p.Poll()
		err := syscall.Kill(-pgid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// SIGKILLs snapshotted survivors that escaped the group through setsid
func sweepEscapedDescendants(descendants []descendantSnapshot, pgid int) {
	for _, d := range descendants {
		if !d.running() {
			continue
		}
		if g, err := syscall.Getpgid(d.pid); err == nil && g == pgid {
			continue
		}
		syscall.Kill(d.pid, syscall.SIGKILL)
	}
}

// Confirms the group leader is still the process spawned before signalling the whole group
func leaderIsOurs(pgid int, expectedStart string) bool {
	if pgid <= 0 {
		return false
	}
	if expectedStart == "" {
		return true
	}
	expected, err := strconv.ParseInt(expectedStart, 10, 64)
	if err != nil {
		return true
	}
	current, ok := processStartTime(pgid)
	if !ok {
		return true
	}
	return startTimeFingerprintsMatch(expected, current)
}

// KILLs the wrapper and its snapshotted descendants by pid
func killKnownPids(p *ExecProcess, descendants []descendantSnapshot) {
	p.Kill()
	for _, d := range descendants {
		if d.running() {
			syscall.Kill(d.pid, syscall.SIGKILL)
		}
	}
}

// TERMs the group, waits, KILLs, then sweeps setsid escapees with a pid reuse guard
func killLocalProcessTree(p *ExecProcess) error {
	pid := p.Pid()
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		if p.pgid == 0 {
			return err
		}
		pgid = p.pgid
	}
	if !leaderIsOurs(pgid, p.pgidStart) {
		sweepEscapedDescendants(snapshotDescendants(pid), pgid)
		return nil
	}
	descendants := snapshotDescendants(pid)
	if pgid == syscall.Getpgrp() {
		killKnownPids(p, descendants)
	} else {
		err := syscall.Kill(-pgid, syscall.SIGTERM)
		switch {
		case err == nil:
			if !waitForGroupExit(p, pgid, time.Second) {
				if !leaderIsOurs(pgid, p.pgidStart) {
					return nil
				}
				syscall.Kill(-pgid, syscall.SIGKILL)
				waitForGroupExit(p, pgid, 2*time.Second)
				p.Wait(0.2)
			}
		case errors.Is(err, syscall.ESRCH):
		case errors.Is(err, syscall.EPERM):
			killKnownPids(p, descendants)
		default:
			return err
		}
	}
	sweepEscapedDescendants(descendants, pgid)
	return nil
}

// SIGKILLs the whole group with no TERM grace or wait, guarded against a recycled leader
func forceKillLocalProcessTree(p *ExecProcess) {
	pgid := p.pgid
	if pgid == 0 {
		if g, err := syscall.Getpgid(p.Pid()); err == nil {
			pgid = g
		}
	}
	if pgid != 0 && pgid != syscall.Getpgrp() && leaderIsOurs(pgid, p.pgidStart) {
		syscall.Kill(-pgid, syscall.SIGKILL)
	}
	p.Kill()
}
