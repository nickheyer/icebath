//go:build !windows

package pty

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	creack "github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// Time an exited shell waits for its output to drain before the terminal is torn down
const destroySocketTimeout = 200 * time.Millisecond

func accessExecutable(path string) bool { return unix.Access(path, unix.X_OK) == nil }

// Starts the child in its own session with no stdio so it outlives the desktop
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

type unixPty struct {
	cmd       *exec.Cmd
	master    *os.File
	writes    *writeQueue
	closeOnce sync.Once
	mu        sync.Mutex
	reaped    bool
}

// Starts command on a fresh pseudo terminal with PWD and TERM set, streaming output and then the exit
func spawnPty(command string, args []string, o spawnOptions, onData func(string), onExit func(ExitPayload)) (ptyProcess, error) {
	env := make(map[string]string, len(o.Env)+2)
	for k, v := range o.Env {
		env[k] = v
	}
	env["PWD"] = o.Cwd
	env["TERM"] = o.Name
	cmd := exec.Command(command, args...)
	cmd.Dir = o.Cwd
	cmd.Env = make([]string, 0, len(env))
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	master, err := creack.StartWithSize(cmd, &creack.Winsize{Cols: uint16(o.Cols), Rows: uint16(o.Rows)})
	if err != nil {
		return nil, err
	}
	p := &unixPty{cmd: cmd, master: master, writes: newWriteQueue(master)}
	drained := make(chan struct{})
	go func() {
		pumpUTF8(master, onData)
		close(drained)
	}()
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.reaped = true
		p.mu.Unlock()
		code, signal := 0, 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
				if ws.Exited() {
					code = ws.ExitStatus()
				}
				if ws.Signaled() {
					signal = int(ws.Signal())
				}
			}
		}
		select {
		case <-drained:
		case <-time.After(destroySocketTimeout):
		}
		p.closeMaster()
		<-drained
		sig := strconv.Itoa(signal)
		onExit(ExitPayload{Code: &code, Signal: &sig})
	}()
	return p, nil
}

func (p *unixPty) closeMaster() {
	p.closeOnce.Do(func() {
		p.writes.close()
		p.master.Close()
	})
}

// Queues input for the shell without waiting on the kernel buffer
func (p *unixPty) Write(data string) { p.writes.push(data) }

func (p *unixPty) Resize(cols, rows int) error {
	return creack.Setsize(p.master, &creack.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// Hangs up the shell process while it is still running
func (p *unixPty) Kill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.reaped {
		_ = p.cmd.Process.Signal(syscall.SIGHUP)
	}
}

func (p *unixPty) Pid() int { return p.cmd.Process.Pid }
