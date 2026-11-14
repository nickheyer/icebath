//go:build !windows

package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

const ptyTerminateDelay = 100 * time.Millisecond

const ptyUnavailableReason = "pseudo-terminals are not available on this system"

func ptyAvailable() bool { return true }

func ptyMissing(error) bool { return false }

type unixPTY struct {
	cmd       *exec.Cmd
	master    *os.File
	done      chan struct{}
	code      *int
	closeOnce sync.Once
}

func startPTY(argv []string, cwd string, env []string) (ptyJob, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = env
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: ptyRows, Cols: ptyCols})
	if err != nil {
		return nil, err
	}
	j := &unixPTY{cmd: cmd, master: master, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() {
			code := cmd.ProcessState.ExitCode()
			j.code = &code
		}
		close(j.done)
	}()
	return j, nil
}

func (j *unixPTY) pid() int { return j.cmd.Process.Pid }

func (j *unixPTY) read(p []byte) (int, error) { return j.master.Read(p) }

func (j *unixPTY) write(p []byte) (int, error) { return j.master.Write(p) }

func (j *unixPTY) sendEOF() error {
	eof := byte(4)
	if tio, err := unix.IoctlGetTermios(syscall.Stdout, ioctlGetTermios); err == nil {
		eof = tio.Cc[unix.VEOF]
	}
	_, err := j.master.Write([]byte{eof})
	return err
}

func (j *unixPTY) signal(sig syscall.Signal) error {
	if !j.alive() {
		return nil
	}
	if err := syscall.Kill(j.cmd.Process.Pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (j *unixPTY) terminate(force bool) (bool, error) {
	if !j.alive() {
		return true, nil
	}
	steps := []syscall.Signal{syscall.SIGHUP, syscall.SIGCONT, syscall.SIGINT}
	if force {
		steps = append(steps, syscall.SIGKILL)
	}
	for _, sig := range steps {
		if err := j.signal(sig); err != nil {
			time.Sleep(ptyTerminateDelay)
			if !j.alive() {
				return true, nil
			}
			return false, err
		}
		time.Sleep(ptyTerminateDelay)
		if !j.alive() {
			return true, nil
		}
	}
	return false, nil
}

func signalTerminate(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

func (j *unixPTY) alive() bool {
	select {
	case <-j.done:
		return false
	default:
		return true
	}
}

func (j *unixPTY) wait(ctx context.Context) (*int, error) {
	select {
	case <-j.done:
		return j.code, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (j *unixPTY) exitCode() (*int, bool) {
	select {
	case <-j.done:
		return j.code, true
	default:
		return nil, false
	}
}

func (j *unixPTY) close() error {
	var err error
	j.closeOnce.Do(func() { err = j.master.Close() })
	return err
}

func (j *unixPTY) answersQueries() bool { return true }
