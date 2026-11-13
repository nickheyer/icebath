//go:build windows

package pty

import (
	"context"
	"os"
	"os/exec"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/UserExistsError/conpty"
	"golang.org/x/sys/windows"
)

// Quiet time after the shell exits before the pseudo console is closed and the exit reported
const flushDataInterval = 1000 * time.Millisecond

func accessExecutable(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Starts the child detached in its own process group with no stdio so it outlives the desktop
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
}

type windowsPty struct {
	cpty      *conpty.ConPty
	writes    *writeQueue
	closeOnce sync.Once
	mu        sync.Mutex
	exited    bool
}

// Starts command on a ConPTY pseudo console, streaming output and reporting the exit once output goes quiet
func spawnPty(command string, args []string, o spawnOptions, onData func(string), onExit func(ExitPayload)) (ptyProcess, error) {
	env := make([]string, 0, len(o.Env))
	for k, v := range o.Env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	cpty, err := conpty.Start(windows.ComposeCommandLine(append([]string{command}, args...)),
		conpty.ConPtyDimensions(o.Cols, o.Rows), conpty.ConPtyWorkDir(o.Cwd), conpty.ConPtyEnv(env))
	if err != nil {
		return nil, err
	}
	p := &windowsPty{cpty: cpty, writes: newWriteQueue(cpty)}
	activity := make(chan struct{}, 1)
	drained := make(chan struct{})
	go func() {
		pumpUTF8(cpty, func(data string) {
			onData(data)
			select {
			case activity <- struct{}{}:
			default:
			}
		})
		close(drained)
	}()
	go func() {
		code, _ := cpty.Wait(context.Background())
		p.mu.Lock()
		p.exited = true
		p.mu.Unlock()
		quiet := time.NewTimer(flushDataInterval)
		for waiting := true; waiting; {
			select {
			case <-activity:
				quiet.Reset(flushDataInterval)
			case <-quiet.C:
				waiting = false
			case <-drained:
				waiting = false
			}
		}
		p.close()
		<-drained
		exitCode := int(code)
		onExit(ExitPayload{Code: &exitCode})
	}()
	return p, nil
}

func (p *windowsPty) close() {
	p.closeOnce.Do(func() {
		p.writes.close()
		_ = p.cpty.Close()
	})
}

// Queues input for the shell without waiting on the pipe
func (p *windowsPty) Write(data string) { p.writes.push(data) }

func (p *windowsPty) Resize(cols, rows int) error { return p.cpty.Resize(cols, rows) }

// Terminates the shell process, the pseudo console close then ending every other attached process
func (p *windowsPty) Kill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited {
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(p.cpty.Pid()))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	_ = windows.TerminateProcess(h, 1)
}

func (p *windowsPty) Pid() int { return p.cpty.Pid() }
