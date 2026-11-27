//go:build !windows

package terminal

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/nickheyer/icebath/pkg/term"
)

// Enter as a program on a pseudo terminal reads it
const ptyEnter = "\r"

// Pauses after each termination signal and after closing the master
const ptyPause = 100 * time.Millisecond

var errNotTerminated = errors.New("The process could not be stopped.")

// Program on a pseudo terminal, for background jobs driven through their prompts
type ptyProc struct {
	cmd    *exec.Cmd
	master *os.File
	done   chan struct{}
	code   *int
	mu     sync.Mutex
	closed bool
	eof    byte
}

// Starts argv on a fresh pseudo terminal of rows by cols as the leader of a session of its own
func startPty(argv []string, dir string, env []string, rows, cols int) (*ptyProc, error) {
	master, slave, err := term.OpenPty()
	if err != nil {
		return nil, err
	}
	if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: uint16(rows), Col: uint16(cols)}); err != nil {
		master.Close()
		slave.Close()
		return nil, err
	}
	eof := byte(4)
	if tio, err := unix.IoctlGetTermios(int(slave.Fd()), termiosRequest); err == nil {
		eof = tio.Cc[unix.VEOF]
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		master.Close()
		slave.Close()
		return nil, err
	}
	slave.Close()
	p := &ptyProc{cmd: cmd, master: master, done: make(chan struct{}), eof: eof}
	go func() {
		cmd.Wait()
		if state := cmd.ProcessState; state != nil && signalOf(state) == 0 {
			code := state.ExitCode()
			p.code = &code
		}
		close(p.done)
	}()
	return p, nil
}

func (p *ptyProc) Pid() int { return p.cmd.Process.Pid }

// Output from the master, io.EOF once nothing holds the terminal
func (p *ptyProc) Read(buf []byte) (int, error) {
	n, err := p.master.Read(buf)
	if err != nil && (errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed)) {
		return n, io.EOF
	}
	return n, err
}

func (p *ptyProc) Write(data []byte) error {
	_, err := p.master.Write(data)
	return err
}

func (p *ptyProc) Alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *ptyProc) Wait() { <-p.done }

// Exit code of a program that exited, nil while it runs or after a signal killed it
func (p *ptyProc) ExitStatus() *int {
	if p.Alive() {
		return nil
	}
	return p.code
}

// Signals the program's whole session, the program alone when that fails
func (p *ptyProc) signal(sig syscall.Signal) {
	if !p.Alive() {
		return
	}
	if pid := p.cmd.Process.Pid; syscall.Kill(-pid, sig) != nil {
		syscall.Kill(pid, sig)
	}
}

// HUP, CONT and INT, then TERM, then KILL when forced; true once the program is gone
func (p *ptyProc) Terminate(force bool) bool {
	for _, step := range [][]syscall.Signal{{syscall.SIGHUP, syscall.SIGCONT, syscall.SIGINT}, {syscall.SIGTERM}} {
		if !p.Alive() {
			return true
		}
		for _, sig := range step {
			p.signal(sig)
		}
		time.Sleep(ptyPause)
	}
	if !p.Alive() {
		return true
	}
	if force {
		p.signal(syscall.SIGKILL)
		time.Sleep(ptyPause)
		return !p.Alive()
	}
	return false
}

// Writes the terminal's end of file character so a reading program sees end of input
func (p *ptyProc) SendEOF() error {
	return p.Write([]byte{p.eof})
}

// Closes the master and terminates a program still running, erroring when it survives
func (p *ptyProc) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.master.Close()
	time.Sleep(ptyPause)
	if p.Alive() && !p.Terminate(true) {
		return errNotTerminated
	}
	p.closed = true
	return nil
}
