//go:build !windows

package selfupdate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Inherited lock descriptors as extra files that keep each at its own number in a child
func launcherFiles(list string) []*os.File {
	var files []*os.File
	for _, field := range strings.Split(list, ",") {
		fd, err := strconv.Atoi(field)
		if err != nil || fd < 3 {
			continue
		}
		for len(files) < fd-2 {
			files = append(files, nil)
		}
		files[fd-3] = os.NewFile(uintptr(fd), "fd"+field)
	}
	return files
}

// Exit status as the launcher reports it, 128 plus the signal for a child a signal killed
func launcherExit(state *os.ProcessState) int {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return state.ExitCode()
}

// Leaves the caller's group, starts the custodian in a group of its own and relays signals and its exit status
func reapStandIn(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "custody launcher needs the lock descriptors and a command")
		return 2
	}
	caller := syscall.Getpgrp()
	if caller != os.Getpid() {
		_ = syscall.Setpgid(0, 0)
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	custodian := exec.Command(exe, append([]string{CustodyCustodianEntry, strconv.Itoa(caller), strconv.Itoa(os.Getpid()), args[0]}, args[1:]...)...)
	custodian.Stdin, custodian.Stdout, custodian.Stderr = os.Stdin, os.Stdout, os.Stderr
	custodian.ExtraFiles = launcherFiles(args[0])
	custodian.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	if err := custodian.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	go func() {
		for s := range signals {
			_ = custodian.Process.Signal(s)
		}
	}()
	_ = custodian.Wait()
	return launcherExit(custodian.ProcessState)
}

// Runs the command in the caller's group holding the lock descriptors until no descendant of it is left
func reapCustodian(args []string) int {
	if len(args) < 4 {
		fmt.Fprintln(os.Stderr, "custody custodian needs the caller group, stand-in, lock descriptors and a command")
		return 2
	}
	caller, _ := strconv.Atoi(args[0])
	standIn, _ := strconv.Atoi(args[1])
	me := os.Getpid()
	reaper := custodySubreaper()
	p := exec.Command(args[3], args[4:]...)
	p.Stdin, p.Stdout, p.Stderr = os.Stdin, os.Stdout, os.Stderr
	p.ExtraFiles = launcherFiles(args[2])
	p.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: caller}
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	if err := p.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var got atomic.Bool
	go func() {
		for s := range signals {
			got.Store(true)
			_ = p.Process.Signal(s)
		}
	}()
	exited := make(chan struct{})
	go func() { _ = p.Wait(); close(exited) }()
	seen := map[int]string{}
	var deadline time.Time
	for running := true; running; {
		select {
		case <-exited:
			running = false
			continue
		case <-time.After(500 * time.Millisecond):
		}
		if !reaper {
			sampleTree(seen, p.Process.Pid, true)
		}
		if got.Load() || syscall.Getppid() != standIn {
			if deadline.IsZero() {
				deadline = time.Now().Add(10 * time.Second)
			}
			if time.Now().After(deadline) {
				_ = p.Process.Kill()
			}
		}
	}
	code := launcherExit(p.ProcessState)
	end := time.Now().Add(10 * time.Second)
	for time.Now().Before(end) {
		var live []int
		if reaper {
			live = childrenOf(me)
		} else {
			live = sampleTree(seen, 0, false)
		}
		if len(live) == 0 {
			break
		}
		for _, pid := range live {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		for reaper {
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
			if err != nil || pid == 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return code
}

// Processes whose parent is pid read from /proc
func childrenOf(pid int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var found []int
	for _, entry := range entries {
		child, err := strconv.Atoi(entry.Name())
		if err != nil || !isASCIIDigits(entry.Name()) {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		close := strings.LastIndexByte(string(stat), ')')
		if close < 0 || close+2 > len(stat) {
			continue
		}
		fields := strings.Fields(string(stat[close+2:]))
		if len(fields) > 1 {
			if ppid, err := strconv.Atoi(fields[1]); err == nil && ppid == pid {
				found = append(found, child)
			}
		}
	}
	return found
}

func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

type psRow struct {
	ppid  int
	start string
}

// Live non zombie processes from ps with their parent and start time
func processTable() map[int]psRow {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=,stat=,lstart=")
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return map[int]psRow{}
	}
	if ctx.Err() != nil {
		return map[int]psRow{}
	}
	rows := map[int]psRow{}
	for _, line := range strings.Split(pystr.DecodeUTF8Replace(out), "\n") {
		parts := splitWhitespace(line, 3)
		if len(parts) != 4 || !isASCIIDigits(parts[0]) || !isASCIIDigits(parts[1]) || strings.HasPrefix(parts[2], "Z") {
			continue
		}
		pid, _ := strconv.Atoi(parts[0])
		ppid, _ := strconv.Atoi(parts[1])
		rows[pid] = psRow{ppid: ppid, start: pystr.Strip(parts[3])}
	}
	return rows
}

// str.split(None, maxsplit) over ASCII whitespace runs
func splitWhitespace(s string, maxsplit int) []string {
	var parts []string
	s = strings.TrimLeft(s, " \t\r\n\v\f")
	for s != "" {
		if len(parts) == maxsplit {
			parts = append(parts, s)
			break
		}
		end := strings.IndexAny(s, " \t\r\n\v\f")
		if end < 0 {
			parts = append(parts, s)
			break
		}
		parts = append(parts, s[:end])
		s = strings.TrimLeft(s[end:], " \t\r\n\v\f")
	}
	return parts
}

// Records the descendants of root and returns every recorded process whose start time still matches
func sampleTree(seen map[int]string, root int, hasRoot bool) []int {
	rows := processTable()
	matches := func(pid int, start string) bool {
		row, ok := rows[pid]
		return ok && row.start == start
	}
	tree := map[int]bool{}
	for pid, start := range seen {
		if matches(pid, start) {
			tree[pid] = true
		}
	}
	if hasRoot {
		tree[root] = true
	}
	for grew := true; grew; {
		grew = false
		for pid, row := range rows {
			if tree[row.ppid] && !tree[pid] {
				tree[pid] = true
				seen[pid] = row.start
				grew = true
			}
		}
	}
	var live []int
	for pid, start := range seen {
		if matches(pid, start) {
			live = append(live, pid)
		}
	}
	return live
}
