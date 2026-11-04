//go:build !windows

package tools

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
)

// POSIX half of the process identity layer, group probes, the supervisor shell and signals

// Shell the supervisor runs under, the Android system shell when /bin/sh is absent
var posixSh = func() string {
	if _, err := os.Stat("/bin/sh"); err == nil {
		return "/bin/sh"
	}
	return "/system/bin/sh"
}()

// Reports whether no member of pgid is left, an ambiguous probe keeps the group covered
func processGroupGone(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return errors.Is(err, syscall.ESRCH)
}

// Sends TERM or KILL to the whole group, errNoSuchProcess when the group is already gone
func signalProcessGroup(pgid int, force bool) error {
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	err := syscall.Kill(-pgid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return errNoSuchProcess
	}
	return err
}

// Live supervisor shell with its control pipe
type procSupervisor struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	done   chan struct{}
	code   int
	closeM sync.Mutex
	closed bool
}

func (s *procSupervisor) write(payload string) error {
	if payload == "" {
		return nil
	}
	s.closeM.Lock()
	defer s.closeM.Unlock()
	if s.closed {
		return os.ErrClosed
	}
	_, err := io.WriteString(s.stdin, payload)
	return err
}

func (s *procSupervisor) closeStdin() error {
	s.closeM.Lock()
	defer s.closeM.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.stdin.Close()
}

func (s *procSupervisor) poll() (bool, int) {
	select {
	case <-s.done:
		return true, s.code
	default:
		return false, 0
	}
}

func (s *procSupervisor) wait() int {
	<-s.done
	return s.code
}

func (s *procSupervisor) pid() int { return s.cmd.Process.Pid }

// Spawns the supervisor shell in its own session with only its control pipe shared
func startSupervisorProcess() (supervisorHandle, error) {
	cmd := exec.Command(posixSh, "-c", supervisorScript, supervisorArgv0, strconv.Itoa(syscall.Getpgrp()))
	cmd.Stdout = nil
	cmd.Stderr = nil
	startNewSession(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := &procSupervisor{cmd: cmd, stdin: stdin, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		if cmd.ProcessState != nil {
			if sig := signalOf(cmd.ProcessState); sig != 0 {
				s.code = -sig
			} else {
				s.code = cmd.ProcessState.ExitCode()
			}
		} else if err != nil {
			s.code = -1
		}
		close(s.done)
	}()
	return s, nil
}

// Windows only, POSIX children die through the supervisor instead
func attachSelfToKillOnCloseJob() bool { return false }

// Process facts read from the running kernel
type osProcessProber struct{}

func (osProcessProber) Running(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func (osProcessProber) Terminate(pid int) error { return signalPid(pid, syscall.SIGTERM) }

func (osProcessProber) Kill(pid int) error { return signalPid(pid, syscall.SIGKILL) }

func signalPid(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return errNoSuchProcess
	}
	err := syscall.Kill(pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return errNoSuchProcess
	}
	return err
}
