//go:build linux || darwin

package term

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Program running on a pseudo terminal
type Process struct {
	cmd  *exec.Cmd
	pty  *os.File
	done chan struct{}
	exit Exit
}

// Starts the command on a fresh pseudo terminal of the given size
func startProcess(c Command, cols, rows int) (*Process, error) {
	master, slave, err := openPty()
	if err != nil {
		return nil, err
	}
	if err := setSize(master, cols, rows); err != nil {
		master.Close()
		slave.Close()
		return nil, err
	}
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		master.Close()
		slave.Close()
		return nil, err
	}
	slave.Close()
	p := &Process{cmd: cmd, pty: master, done: make(chan struct{})}
	go p.wait()
	return p, nil
}

// Waits for the program and records how it ended
func (p *Process) wait() {
	err := p.cmd.Wait()
	var exit Exit
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		exit.Code = ee.ExitCode()
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			exit.Code = 128 + int(ws.Signal())
			exit.Signal = ws.Signal().String()
		}
	default:
		exit.Code, exit.Err = 1, err
	}
	p.exit = exit
	close(p.done)
}

// Output of the program
func (p *Process) Read(b []byte) (int, error) { return p.pty.Read(b) }

// Input to the program
func (p *Process) Write(b []byte) (int, error) { return p.pty.Write(b) }

// Tells the program the terminal is cols by rows
func (p *Process) Resize(cols, rows int) error { return setSize(p.pty, cols, rows) }

// Process id of the program
func (p *Process) Pid() int { return p.cmd.Process.Pid }

// Working directory of the program as the kernel knows it, empty where that cannot be read
func (p *Process) Cwd() string {
	dir, err := os.Readlink("/proc/" + strconv.Itoa(p.Pid()) + "/cwd")
	if err != nil {
		return ""
	}
	return dir
}

// Hangs up the terminal, then kills the program and everything under it when it lingers
func (p *Process) Close() {
	p.pty.Close()
	select {
	case <-p.done:
		return
	case <-time.After(closeGrace):
	}
	proc.KillTree(p.Pid())
	<-p.done
}

// Shell named by SHELL, else the plain shell of the system
func defaultShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		if _, err := os.Stat(sh); err == nil {
			return sh
		}
	}
	return "/bin/sh"
}

// Sets the terminal size the kernel reports to the program
func setSize(f *os.File, cols, rows int) error {
	ws := &unix.Winsize{Col: uint16(cols), Row: uint16(rows), Xpixel: uint16(cols * cellPixelW), Ypixel: uint16(rows * cellPixelH)}
	return unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, ws)
}
