package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/treekill"
)

// ChildProcess is a retained child handle the tree kill helpers signal, wait on and reap
type ChildProcess interface {
	Pid() int
	Kill() error
	WaitExit(timeout time.Duration) bool
}

// DrainableChild is a child handle that also hands back what its captured pipes still hold
type DrainableChild interface {
	ChildProcess
	Communicate(timeout time.Duration) (stdout, stderr []byte, ok bool)
}

// Shared tree kill primitive and the legacy taskkill runner, swapped by tests
var (
	deadlineKillTree  = treekill.KillProcessTree
	legacyTaskkillRun = runLegacyTaskkill
)

// KillChildTree terminates proc and its descendants on both platforms and swallows every failure
func KillChildTree(proc ChildProcess, grace time.Duration) {
	delegated, finished := delegateChildTreeKill(proc, grace)
	if !delegated {
		legacyKillChildTree(proc)
		return
	}
	if finished {
		return
	}
	_ = proc.Kill()
}

// Runs the shared tree kill with the graced group sweep, false when a step failed and the legacy kill must run
func delegateChildTreeKill(proc ChildProcess, grace time.Duration) (delegated, finished bool) {
	defer func() {
		if r := recover(); r != nil {
			delegated, finished = false, false
		}
	}()
	pid := proc.Pid()
	if grace > 0 && !isWindowsHost {
		pgid, err := getpgid(pid)
		leadsGroup := err == nil && pgid == pid
		if err != nil && !errors.Is(err, syscall.ESRCH) {
			return false, false
		}
		deadlineKillTree(pid, syscall.SIGTERM)
		if proc.WaitExit(grace) {
			if leadsGroup {
				if err := killpg(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
					return false, false
				}
			}
			return true, true
		}
	}
	deadlineKillTree(pid, 0)
	return true, false
}

// Kills the group proc leads and proc itself, then taskkills the tree on Windows without an identity probe
func legacyKillChildTree(proc ChildProcess) {
	pid := proc.Pid()
	if !isWindowsHost {
		if pgid, err := getpgid(pid); err == nil && pgid == pid {
			_ = killpg(pgid, syscall.SIGKILL)
		}
	}
	_ = proc.Kill()
	if isWindowsHost {
		legacyTaskkillRun([]string{"taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)})
	}
}

// Runs taskkill hidden with null stdio for at most two seconds, ignoring every failure
func runLegacyTaskkill(argv []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.SysProcAttr = windowsHideAttr()
	_ = cmd.Run()
}

// KillAndDrain tree kills proc then returns what its pipes still hold within timeout, false when a survivor holds them
func KillAndDrain(proc DrainableChild, timeout time.Duration) (stdout, stderr []byte, ok bool) {
	KillChildTree(proc, 0)
	return proc.Communicate(timeout)
}

// CmdChild is a started command reaped by one goroutine so its exit can be awaited with a timeout
type CmdChild struct {
	Cmd    *exec.Cmd
	exited chan struct{}
	err    error
}

// WatchCmd reaps an already started command in the background
func WatchCmd(cmd *exec.Cmd) *CmdChild {
	c := &CmdChild{Cmd: cmd, exited: make(chan struct{})}
	go func() {
		c.err = cmd.Wait()
		close(c.exited)
	}()
	return c
}

// Pid is the child process id
func (c *CmdChild) Pid() int { return c.Cmd.Process.Pid }

// Kill sends the child alone a kill, an already reaped child is no error as with Popen.kill
func (c *CmdChild) Kill() error {
	select {
	case <-c.exited:
		return nil
	default:
	}
	if err := c.Cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// WaitExit reports whether the child exited within timeout
func (c *CmdChild) WaitExit(timeout time.Duration) bool {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-c.exited:
		return true
	case <-t.C:
		return false
	}
}

// Done is closed once the child has been reaped
func (c *CmdChild) Done() <-chan struct{} { return c.exited }

// Err blocks until the child is reaped and returns what Wait returned
func (c *CmdChild) Err() error {
	<-c.exited
	return c.err
}

// PipedChild is a started command whose stdout and stderr pipes are read to EOF in the background
type PipedChild struct {
	*CmdChild
	stdout, stderr bytes.Buffer
	drained        chan struct{}
}

// StartPiped starts cmd with stdout and stderr on pipes that a surviving descendant can keep open
func StartPiped(cmd *exec.Cmd) (*PipedChild, error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	err = cmd.Start()
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		return nil, err
	}
	p := &PipedChild{CmdChild: WatchCmd(cmd), drained: make(chan struct{})}
	var wg sync.WaitGroup
	wg.Add(2)
	for _, pair := range []struct {
		r   *os.File
		buf *bytes.Buffer
	}{{outR, &p.stdout}, {errR, &p.stderr}} {
		go func() {
			defer wg.Done()
			_, _ = io.Copy(pair.buf, pair.r)
			pair.r.Close()
		}()
	}
	go func() {
		wg.Wait()
		close(p.drained)
	}()
	return p, nil
}

// Communicate waits within timeout for both pipes to reach EOF and the child to exit
func (p *PipedChild) Communicate(timeout time.Duration) (stdout, stderr []byte, ok bool) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for _, ch := range []<-chan struct{}{p.drained, p.exited} {
		select {
		case <-ch:
		case <-t.C:
			return nil, nil, false
		}
	}
	return p.stdout.Bytes(), p.stderr.Bytes(), true
}

// Zombie by proc stat field three, or by ps state where proc is absent, a stat that is not UTF-8 errors
func posixIsZombie(pid int) (bool, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err == nil {
		text, err := pystr.DecodeUTF8(raw)
		if err != nil {
			return false, err
		}
		fields := pystr.SplitWhitespace(text, -1)
		return len(fields) > 2 && fields[2] == "Z", nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
	if err != nil || ctx.Err() != nil {
		return false, nil
	}
	return strings.HasPrefix(pystr.Strip(pystr.DecodeUTF8Replace(out)), "Z"), nil
}
