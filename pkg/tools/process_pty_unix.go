//go:build !windows

package tools

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Pseudo terminal child for pty=true background spawns, the ptyprocess contract on POSIX

// Pauses ptyprocess takes after each termination signal and after closing the master
const (
	ptyDelayAfterTerminate = 100 * time.Millisecond
	ptyDelayAfterClose     = 100 * time.Millisecond
)

var errPtyNotTerminated = errors.New("Could not terminate the child.")

type ptyProcess struct {
	cmd      *exec.Cmd
	master   *os.File
	done     chan struct{}
	exitCode *int
	signaled int
	closeMu  sync.Mutex
	closed   bool
	eofChar  byte
}

// Starts argv on a fresh pseudo terminal of rows by cols as its own session leader
func spawnPty(argv []string, cwd string, env map[string]string, rows, cols int) (*ptyProcess, error) {
	master, slave, err := openProcessPty()
	if err != nil {
		return nil, err
	}
	ws := &unix.Winsize{Row: uint16(rows), Col: uint16(cols)}
	if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, ws); err != nil {
		master.Close()
		slave.Close()
		return nil, err
	}
	eof := byte(4)
	if tio, err := unix.IoctlGetTermios(int(slave.Fd()), ioctlGetTermiosRequest); err == nil {
		eof = byte(tio.Cc[unix.VEOF])
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = envSlice(env)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		master.Close()
		slave.Close()
		return nil, err
	}
	slave.Close()
	p := &ptyProcess{cmd: cmd, master: master, done: make(chan struct{}), eofChar: eof}
	go func() {
		err := cmd.Wait()
		if cmd.ProcessState != nil {
			if sig := signalOf(cmd.ProcessState); sig != 0 {
				p.signaled = sig
			} else {
				code := cmd.ProcessState.ExitCode()
				p.exitCode = &code
			}
		} else if err != nil {
			p.signaled = -1
		}
		close(p.done)
	}()
	return p, nil
}

func (p *ptyProcess) Pid() int { return p.cmd.Process.Pid }

// Reads output from the master, io.EOF once every holder of the slave is gone
func (p *ptyProcess) Read(buf []byte) (int, error) {
	n, err := p.master.Read(buf)
	if err != nil {
		if errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed) {
			return n, io.EOF
		}
		return n, err
	}
	return n, nil
}

func (p *ptyProcess) Write(data []byte) error {
	_, err := p.master.Write(data)
	return err
}

func (p *ptyProcess) IsAlive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// Blocks until the child is reaped
func (p *ptyProcess) Wait() {
	<-p.done
}

// Exit status of a normally exited child, nil while running or after a signal death
func (p *ptyProcess) ExitStatus() *int {
	select {
	case <-p.done:
		return p.exitCode
	default:
		return nil
	}
}

func (p *ptyProcess) kill(sig syscall.Signal) {
	if p.IsAlive() {
		syscall.Kill(p.cmd.Process.Pid, sig)
	}
}

// HUP, CONT, INT, then TERM, then KILL when forced, true once the child is gone
func (p *ptyProcess) Terminate(force bool) bool {
	if !p.IsAlive() {
		return true
	}
	p.kill(syscall.SIGHUP)
	p.kill(syscall.SIGCONT)
	p.kill(syscall.SIGINT)
	time.Sleep(ptyDelayAfterTerminate)
	if !p.IsAlive() {
		return true
	}
	p.kill(syscall.SIGTERM)
	time.Sleep(ptyDelayAfterTerminate)
	if !p.IsAlive() {
		return true
	}
	if force {
		p.kill(syscall.SIGKILL)
		time.Sleep(ptyDelayAfterTerminate)
		return !p.IsAlive()
	}
	return false
}

// Writes the terminal's EOF character so a canonical reader sees end of file
func (p *ptyProcess) SendEOF() error {
	return p.Write([]byte{p.eofChar})
}

// Closes the master and terminates a child that is still alive, erroring when it survives
func (p *ptyProcess) Close() error {
	p.closeMu.Lock()
	defer p.closeMu.Unlock()
	if p.closed {
		return nil
	}
	p.master.Close()
	time.Sleep(ptyDelayAfterClose)
	if p.IsAlive() && !p.Terminate(true) {
		return errPtyNotTerminated
	}
	p.closed = true
	return nil
}
