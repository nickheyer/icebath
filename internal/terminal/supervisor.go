//go:build !windows

package terminal

import (
	_ "embed"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Shell that reaps the process groups it was told of once icebath is gone, however it went
//
//go:embed supervisor.sh
var supervisorScript string

const supervisorName = "icebath-process-supervisor"

// A supervisor that dies sooner than this after starting may not be able to stay up
const supervisorMinUptime = 5 * time.Second

// An idle supervisor exits this long after its last group was released
const supervisorIdle = 30 * time.Second

// Keeps the process groups icebath started dying with it: a resident shell holding a pipe only icebath writes, which kills every group
// still registered once the pipe closes, icebath having exited or been killed
type Supervisor struct {
	mu     sync.Mutex
	shell  *supervisorShell
	groups map[int]bool
	idle   *time.Timer
	// The running shell was itself a quick restart after a signal, so another quick death is not retried at once
	youngRestart bool
}

func NewSupervisor() *Supervisor {
	return &Supervisor{groups: map[int]bool{}}
}

type supervisorShell struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	done   chan struct{}
	code   int
	mu     sync.Mutex
	closed bool
}

func (s *supervisorShell) write(payload string) error {
	if payload == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}
	_, err := io.WriteString(s.stdin, payload)
	return err
}

func (s *supervisorShell) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.stdin.Close()
	}
}

func (s *supervisorShell) exited() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// Starts the shell in a session of its own, sharing nothing with icebath but its control pipe
func startSupervisorShell() (*supervisorShell, error) {
	shell := "/bin/sh"
	if _, err := os.Stat(shell); err != nil {
		shell = "/system/bin/sh"
	}
	cmd := exec.Command(shell, "-c", supervisorScript, supervisorName, strconv.Itoa(syscall.Getpgrp()))
	NewSession(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := &supervisorShell{cmd: cmd, stdin: stdin, done: make(chan struct{})}
	go func() {
		s.code = exitCode(cmd, cmd.Wait())
		close(s.done)
	}()
	return s, nil
}

// Makes the groups die with icebath however it dies
func (s *Supervisor) Own(pgids ...int) { s.update("register", pgids) }

// Stops covering the groups, icebath having stopped them itself
func (s *Supervisor) Release(pgids ...int) { s.update("unregister", pgids) }

// Sends register or unregister lines to the shell, starting it when needed; a failure never fails a child
func (s *Supervisor) update(verb string, pgids []int) {
	if len(pgids) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, pgid := range pgids {
		if verb == "register" {
			s.groups[pgid] = true
		} else {
			delete(s.groups, pgid)
		}
	}
	stale := s.pruneLocked()
	if s.shell == nil || s.shell.exited() {
		if len(s.groups) > 0 {
			s.startLocked()
		}
		return
	}
	if len(s.groups) > 0 {
		s.stopIdleLocked()
	}
	var payload strings.Builder
	for _, pgid := range sorted(pgids) {
		fmt.Fprintf(&payload, "%s %d\n", verb, pgid)
	}
	for _, pgid := range stale {
		fmt.Fprintf(&payload, "unregister %d\n", pgid)
	}
	if err := s.shell.write(payload.String()); err != nil {
		s.shell = nil
		return
	}
	if len(s.groups) == 0 {
		s.releaseWhenIdleLocked(s.shell)
	}
}

// Forgets groups with no member left, answering them
func (s *Supervisor) pruneLocked() []int {
	var stale []int
	for pgid := range s.groups {
		if GroupGone(pgid) {
			stale = append(stale, pgid)
		}
	}
	for _, pgid := range stale {
		delete(s.groups, pgid)
	}
	slices.Sort(stale)
	return stale
}

// Starts a shell and tells it every group covered now
func (s *Supervisor) startLocked() {
	shell, err := startSupervisorShell()
	if err != nil {
		slog.Debug("Could not start the owner-death supervisor", "error", err)
		s.shell = nil
		return
	}
	s.shell = shell
	s.watch(shell)
	var payload strings.Builder
	for _, pgid := range sorted(mapKeys(s.groups)) {
		fmt.Fprintf(&payload, "register %d\n", pgid)
	}
	if err := shell.write(payload.String()); err != nil {
		s.shell = nil
	}
}

// Restarts a shell that died on its own while groups were covered, unless it can't stay up
func (s *Supervisor) watch(shell *supervisorShell) {
	started, young := time.Now(), s.youngRestart
	go func() {
		<-shell.done
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.shell != shell {
			return
		}
		s.shell = nil
		s.pruneLocked()
		if len(s.groups) == 0 {
			return
		}
		old := time.Since(started) >= supervisorMinUptime
		youngSignal := !old && shell.code < 0 && !young
		restart := old || youngSignal
		what := "it could not stay up, retrying at the next registration"
		if restart {
			what = "restarting it"
		}
		slog.Warn(fmt.Sprintf("owner-death supervisor exited (status %d) with %d process group(s) registered; %s", shell.code, len(s.groups), what))
		if restart {
			s.youngRestart = youngSignal
			s.startLocked()
			s.youngRestart = false
		}
	}()
}

// Closes the shell's pipe once no group was covered for the idle grace
func (s *Supervisor) releaseWhenIdleLocked(shell *supervisorShell) {
	s.stopIdleLocked()
	s.idle = time.AfterFunc(supervisorIdle, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.shell == shell && len(s.groups) == 0 {
			shell.close()
			s.shell = nil
		}
	})
}

func (s *Supervisor) stopIdleLocked() {
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
}

func sorted(values []int) []int {
	out := slices.Clone(values)
	slices.Sort(out)
	return slices.Compact(out)
}

func mapKeys(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	return out
}
