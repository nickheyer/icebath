package terminal

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/proc"
)

// A process of this machine with its output merged on one pipe, what a backend running a host command returns
type HostProc struct {
	Cmd  *exec.Cmd
	out  *os.File
	done chan struct{}
	code int
	// Stdin writer: done once it finished, its failure, and the pipe it writes, closed to release it
	stdinDone chan struct{}
	stdinErr  string
	stdinW    *os.File
	// Group the owner death supervisor covers for it, zero once released
	owned int
	owner *Supervisor
	// Group recorded at the start and its leader's start time
	pgid      int
	pgidStart int64
}

// Starts cmd with stdout and stderr merged on a pipe and stdin fed from the text when not nil
func Start(cmd *exec.Cmd, stdin *string) (*HostProc, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = w, w
	var stdinR, stdinW *os.File
	if stdin != nil {
		if stdinR, stdinW, err = os.Pipe(); err != nil {
			r.Close()
			w.Close()
			return nil, err
		}
		cmd.Stdin = stdinR
	}
	proc.HideWindow(cmd)
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		if stdinR != nil {
			stdinR.Close()
			stdinW.Close()
		}
		return nil, err
	}
	w.Close()
	p := &HostProc{Cmd: cmd, out: r, done: make(chan struct{})}
	if stdinR != nil {
		stdinR.Close()
		p.feed(stdinW, *stdin)
	}
	go func() {
		defer close(p.done)
		p.code = exitCode(cmd, cmd.Wait())
	}()
	return p, nil
}

// A process the caller started with pipes of its own, waited on from now so it can be owned, polled and killed with its whole tree
func Watch(cmd *exec.Cmd) *HostProc {
	p := &HostProc{Cmd: cmd, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		p.code = exitCode(cmd, cmd.Wait())
	}()
	return p
}

// Writes the stdin text on a goroutine, then closes it; a child that closed its end early is no failure
func (p *HostProc) feed(w *os.File, data string) {
	p.stdinDone, p.stdinW = make(chan struct{}), w
	go func() {
		defer close(p.stdinDone)
		defer w.Close()
		if _, err := w.WriteString(data); err != nil && !errors.Is(err, os.ErrClosed) && !brokenPipe(err) {
			if path, ok := errors.AsType[*os.PathError](err); ok {
				err = path.Err
			}
			p.stdinErr = err.Error()
		}
	}()
}

// Exit code of a finished command, minus the signal that killed it
func exitCode(cmd *exec.Cmd, err error) int {
	if state := cmd.ProcessState; state != nil {
		if code := state.ExitCode(); code >= 0 {
			return code
		}
		if sig := signalOf(state); sig > 0 {
			return -sig
		}
	}
	if err != nil {
		return 1
	}
	return 0
}

func (p *HostProc) Poll() (int, bool) {
	select {
	case <-p.done:
		return p.code, true
	default:
		return 0, false
	}
}

// Kills the process itself
func (p *HostProc) Kill() error {
	if p.Cmd.Process == nil {
		return nil
	}
	return p.Cmd.Process.Kill()
}

func (p *HostProc) Wait(timeout time.Duration) (int, error) {
	if timeout <= 0 {
		<-p.done
		return p.code, nil
	}
	select {
	case <-p.done:
		return p.code, nil
	case <-time.After(timeout):
		return 0, context.DeadlineExceeded
	}
}

func (p *HostProc) Output() *os.File { return p.out }

func (p *HostProc) Pid() int {
	if p.Cmd.Process == nil {
		return 0
	}
	return p.Cmd.Process.Pid
}

// Whether its stdin is a pipe it was given text on
func (p *HostProc) Piped() bool { return p.stdinDone != nil }

// Waits for the stdin writer and answers its failure; past the timeout the pipe is closed to free a writer the child's descendants keep
// blocked, and a writer still not done reports nothing
func (p *HostProc) joinStdin(timeout time.Duration) string {
	if p.stdinDone == nil {
		return ""
	}
	select {
	case <-p.stdinDone:
		return p.stdinErr
	case <-time.After(timeout):
	}
	p.stdinW.Close()
	select {
	case <-p.stdinDone:
		return p.stdinErr
	case <-time.After(time.Second):
		return ""
	}
}

// Records the group of the process and has the supervisor own it, so it dies with icebath however icebath dies
func (p *HostProc) Own(s *Supervisor) {
	pid := p.Pid()
	if pid == 0 || !groups {
		return
	}
	s.Own(pid)
	p.owned, p.owner = pid, s
	if pgid, err := groupOf(pid); err == nil {
		p.pgid = pgid
	}
	if start, ok := StartTime(pid); ok {
		p.pgidStart = start
	}
}

// Stops covering the process's group, it ended or another owner took it
func (p *HostProc) Release() {
	if owned := p.owned; owned != 0 {
		p.owned = 0
		p.owner.Release(owned)
	}
}

// Kills the process's whole tree: TERM to its group, KILL after a second, then whatever escaped the group
func (p *HostProc) KillTree() {
	if err := killTree(p); err != nil {
		p.Kill()
	}
}

// Kills the process's group at once with no grace, the host exiting next
func (p *HostProc) ForceKill() {
	forceKillTree(p)
}

// Kill, ForceKill and Finish of a Runner whose commands run as processes of this machine, for it to embed
type HostProcs struct{}

// Kills the command's whole process group, what escaped it through setsid included
func (HostProcs) Kill(p Proc) {
	if host, ok := p.(*HostProc); ok {
		host.KillTree()
		return
	}
	p.Kill()
}

func (HostProcs) ForceKill(p Proc) {
	if host, ok := p.(*HostProc); ok {
		host.ForceKill()
		return
	}
	p.Kill()
}

// Stops covering a finished command's group
func (HostProcs) Finish(p Proc) {
	if host, ok := p.(*HostProc); ok {
		host.Release()
	}
}

// A command run by a call into a backend's SDK on a goroutine, its output served through a pipe
type FuncProc struct {
	cancel func()
	done   chan struct{}
	code   int
	out    *os.File
	pid    int
}

// Ids handed to processes that have none of their own
var funcProcs atomic.Int64

// Runs run on a goroutine, its output and exit code becoming the process's; a panic in run reads as exit code 1
func NewFuncProc(run func() (string, int), cancel func()) (*FuncProc, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	p := &FuncProc{cancel: cancel, done: make(chan struct{}), out: r, pid: int(funcProcs.Add(1))}
	go func() {
		defer close(p.done)
		defer w.Close()
		output, code, ok := call(run)
		if !ok {
			p.code = 1
			return
		}
		p.code = code
		w.WriteString(strings.TrimPrefix(output, "\ufeff"))
	}()
	return p, nil
}

func call(run func() (string, int)) (output string, code int, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	output, code = run()
	return output, code, true
}

func (p *FuncProc) Output() *os.File { return p.out }
func (p *FuncProc) Pid() int         { return p.pid }

func (p *FuncProc) Poll() (int, bool) {
	select {
	case <-p.done:
		return p.code, true
	default:
		return 0, false
	}
}

// Cancels the backend call
func (p *FuncProc) Kill() error {
	if p.cancel != nil {
		func() {
			defer func() { recover() }()
			p.cancel()
		}()
	}
	return nil
}

func (p *FuncProc) Wait(timeout time.Duration) (int, error) {
	if timeout <= 0 {
		<-p.done
		return p.code, nil
	}
	select {
	case <-p.done:
		return p.code, nil
	case <-time.After(timeout):
		return 0, context.DeadlineExceeded
	}
}
