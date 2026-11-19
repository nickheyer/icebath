//go:build windows

package tools

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/treekill"
)

// Windows half of the process identity layer, process trees held in job objects and the shared process probes

// Exit codes a terminated or killed tree reports, the numbers of the POSIX signals they stand for
const (
	terminatedExitCode = 15
	killedExitCode     = 9
)

// Reports whether no member of the tree pgid leads is left, an ambiguous probe keeps the tree covered
func processGroupGone(pgid int) bool { return treekill.TreeGone(pgid) }

// Ends every member of the tree pgid leads through its job, errNoSuchProcess when the tree is already gone
func signalProcessGroup(pgid int, force bool) error {
	code := uint32(terminatedExitCode)
	if force {
		code = killedExitCode
	}
	err := treekill.TerminateTree(pgid, code)
	if errors.Is(err, treekill.ErrProcessGone) {
		return errNoSuchProcess
	}
	return err
}

// Owner death supervisor of this process, kill on close set on the job of every registered tree so the kernel ends it when this process dies
type jobSupervisor struct {
	mu     sync.Mutex
	reg    map[int]bool
	done   chan struct{}
	closed bool
}

// Starts the job supervisor, which lives inside this process because the kernel does the reaping
func startSupervisorProcess() (supervisorHandle, error) {
	return &jobSupervisor{reg: map[int]bool{}, done: make(chan struct{})}, nil
}

// Applies register and unregister lines to the jobs of the named trees, skipping malformed lines and this process
func (s *jobSupervisor) write(payload string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}
	for _, line := range strings.Split(payload, "\n") {
		verb, arg, ok := strings.Cut(line, " ")
		pgid, err := strconv.Atoi(arg)
		if !ok || err != nil || pgid <= 0 || pgid == os.Getpid() {
			continue
		}
		switch verb {
		case "register":
			if err := ownTree(pgid, true); err != nil {
				pkgLog().Debug(fmt.Sprintf("could not tie process tree %d to this process's lifetime", pgid), "error", err)
				continue
			}
			s.reg[pgid] = true
		case "unregister":
			if s.reg[pgid] {
				if err := ownTree(pgid, false); err != nil {
					pkgLog().Debug(fmt.Sprintf("could not release process tree %d from this process's lifetime", pgid), "error", err)
				}
			}
			delete(s.reg, pgid)
		}
	}
	treekill.Prune()
	return nil
}

// Ends every tree still registered, as the POSIX supervisor does at the end of its input
func (s *jobSupervisor) closeStdin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	for pgid := range s.reg {
		if err := treekill.TerminateTree(pgid, terminatedExitCode); err != nil && !errors.Is(err, treekill.ErrProcessGone) {
			pkgLog().Debug(fmt.Sprintf("could not end registered process tree %d", pgid), "error", err)
		}
	}
	close(s.done)
	return nil
}

func (s *jobSupervisor) poll() (bool, int) {
	select {
	case <-s.done:
		return true, 0
	default:
		return false, 0
	}
}

func (s *jobSupervisor) wait() int {
	<-s.done
	return 0
}

func (s *jobSupervisor) pid() int { return os.Getpid() }

// Sets or clears kill on close for the tree pgid leads, a running tree started outside a job adopted into one first
func ownTree(pgid int, on bool) error {
	job := treekill.Tracked(pgid)
	if job == nil {
		if !on {
			return nil
		}
		var err error
		if job, err = treekill.Adopt(pgid); err != nil {
			return err
		}
	}
	return job.SetKillOnClose(on)
}

// Process facts read through the Win32 process API
type osProcessProber struct{}

func (osProcessProber) Running(pid int) bool {
	h, err := proc.OpenProcess(pid, windows.PROCESS_QUERY_LIMITED_INFORMATION)
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == proc.StillActive
}

func (osProcessProber) Birth(pid int) (float64, error) { return proc.Birth(pid) }

func (osProcessProber) BootStart(pid int) (float64, bool) { return proc.BootStart(pid) }

func (osProcessProber) Zombie(pid int) bool { return proc.Zombie(pid) }

func (osProcessProber) PPID(pid int) (int, bool) { return proc.PPID(pid) }

// Children of pid, every live member of its job when pid leads a tracked tree and recursion is asked for
func (osProcessProber) Children(pid int, recursive bool) []int {
	if recursive {
		if members, err := treekill.Members(pid); err == nil {
			out := []int{}
			for _, member := range members {
				if member != pid {
					out = append(out, member)
				}
			}
			return out
		}
	}
	table, err := proc.Snapshot()
	if err != nil {
		return nil
	}
	kids := map[int][]int{}
	for _, p := range table {
		kids[p.PPID] = append(kids[p.PPID], p.PID)
	}
	if !recursive {
		return kids[pid]
	}
	seen := map[int]bool{pid: true}
	out := []int{}
	queue := []int{pid}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, k := range kids[cur] {
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, k)
			queue = append(queue, k)
		}
	}
	return out
}

func (osProcessProber) Terminate(pid int) error { return endPid(pid, terminatedExitCode) }

func (osProcessProber) Kill(pid int) error { return endPid(pid, killedExitCode) }

// Ends one process with the exit code standing for the signal
func endPid(pid int, code uint32) error {
	h, err := proc.OpenProcess(pid, windows.PROCESS_TERMINATE)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, code)
}

func (osProcessProber) Cmdline(pid int) ([]string, error) { return proc.Cmdline(pid) }
