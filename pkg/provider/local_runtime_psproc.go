package provider

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// psutil exception classes a process call raises
type psError struct {
	Class string
	Pid   int
	Msg   string
}

func (e *psError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("%s (pid=%d): %s", e.Class, e.Pid, e.Msg)
	}
	return fmt.Sprintf("%s (pid=%d)", e.Class, e.Pid)
}

// Whether err is psutil.NoSuchProcess
func psIsNoSuchProcess(err error) bool {
	var pe *psError
	return errors.As(err, &pe) && pe.Class == "NoSuchProcess"
}

// Process facts the psutil port reads, replaced by tests that step the clock
var (
	lrBirth    = tools.ProcessBirth
	lrCmdline  = tools.ProcessCmdline
	lrChildren = tools.ProcessChildren
)

// psutil.Process of one pid with its creation time captured as the incarnation identity
type psProcess struct {
	pid     int
	created float64
	known   bool
}

// psutil.Process(pid), NoSuchProcess for a missing or out of range pid
func openPSProcess(pid int) (localRuntimeProc, error) {
	if pid < 0 {
		return nil, fmt.Errorf("pid must be a positive integer (got %d)", pid)
	}
	if pid > math.MaxInt32 {
		return nil, &psError{Class: "NoSuchProcess", Pid: pid, Msg: "process PID out of range"}
	}
	p := &psProcess{pid: pid}
	if created, err := lrBirth(pid); err == nil {
		p.created, p.known = created, true
	} else if !psPidExists(pid) {
		return nil, &psError{Class: "NoSuchProcess", Pid: pid, Msg: "process PID not found"}
	}
	return p, nil
}

func (p *psProcess) Pid() int { return p.pid }

// Creation time cached at construction, read again when access was denied then
func (p *psProcess) CreateTime() (float64, error) {
	if p.known {
		return p.created, nil
	}
	created, err := lrBirth(p.pid)
	if err != nil {
		return 0, p.classify(err)
	}
	p.created, p.known = created, true
	return created, nil
}

// NoSuchProcess once the pid is gone, AccessDenied while it lives
func (p *psProcess) classify(err error) error {
	var pe *psError
	if errors.As(err, &pe) {
		return err
	}
	if !psPidExists(p.pid) {
		return &psError{Class: "NoSuchProcess", Pid: p.pid}
	}
	return &psError{Class: "AccessDenied", Pid: p.pid, Msg: err.Error()}
}

// Same pid with the same creation time is still alive
func (p *psProcess) IsRunning() bool {
	created, err := lrBirth(p.pid)
	if err != nil {
		return false
	}
	if !p.known {
		return true
	}
	return created == p.created
}

// NoSuchProcess when the pid now names another incarnation
func (p *psProcess) raiseIfReused() error {
	if !p.IsRunning() {
		return &psError{Class: "NoSuchProcess", Pid: p.pid, Msg: "process no longer exists or its PID has been reused"}
	}
	return nil
}

func (p *psProcess) Exe() (string, error) {
	exe, err := psExe(p.pid)
	if err != nil {
		return "", p.classify(err)
	}
	return exe, nil
}

func (p *psProcess) PPID() (int, error) {
	ppid, err := psPPID(p.pid)
	if err != nil {
		return 0, p.classify(err)
	}
	return ppid, nil
}

// Parent incarnation, nil for the lowest pid or a parent pid reused by a younger process
func (p *psProcess) Parent() (localRuntimeProc, error) {
	if p.pid == psLowestPID {
		return nil, nil
	}
	ppid, err := p.PPID()
	if err != nil {
		return nil, err
	}
	created, err := p.CreateTime()
	if err != nil {
		return nil, err
	}
	parent, err := openPSProcess(ppid)
	if err != nil {
		if psIsNoSuchProcess(err) {
			return nil, nil
		}
		return nil, err
	}
	parentCreated, err := parent.CreateTime()
	if err != nil {
		if psIsNoSuchProcess(err) {
			return nil, nil
		}
		return nil, err
	}
	if parentCreated <= created {
		return parent, nil
	}
	return nil, nil
}

func (p *psProcess) Cmdline() ([]string, error) {
	argv, err := lrCmdline(p.pid)
	if err != nil {
		return nil, p.classify(err)
	}
	return argv, nil
}

// Descendants no older than this process, walking the process table depth first
func (p *psProcess) Children(recursive bool) ([]localRuntimeProc, error) {
	if err := p.raiseIfReused(); err != nil {
		return nil, err
	}
	created, err := p.CreateTime()
	if err != nil {
		return nil, err
	}
	var out []localRuntimeProc
	seen := map[int]bool{}
	stack := []int{p.pid}
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		for _, childPid := range lrChildren(pid) {
			child, err := openPSProcess(childPid)
			if err != nil {
				continue
			}
			childCreated, err := child.CreateTime()
			if err != nil || created > childCreated {
				continue
			}
			out = append(out, child)
			if recursive {
				stack = append(stack, childPid)
			}
		}
		if !recursive {
			break
		}
	}
	return out, nil
}

func (p *psProcess) Terminate() error { return p.signal(false) }

func (p *psProcess) Kill() error { return p.signal(true) }

// Signal sent only to the same incarnation, NoSuchProcess or AccessDenied on failure
func (p *psProcess) signal(kill bool) error {
	if p.pid == 0 {
		return errors.New("preventing sending signal to process with PID 0 as it would affect every process in the process group of the calling process")
	}
	if err := p.raiseIfReused(); err != nil {
		return err
	}
	return psSignal(p.pid, kill)
}

// Blocks until the process exits, TimeoutExpired after timeout
func (p *psProcess) Wait(timeout time.Duration) error {
	if p.pid == 0 {
		return errors.New("can't wait for PID 0")
	}
	if timeout < 0 {
		return fmt.Errorf("timeout must be a positive integer, got %v", timeout.Seconds())
	}
	return psWait(p.pid, timeout)
}

// psutil.TimeoutExpired for a wait that outlived its deadline
func psTimeout(pid int, timeout time.Duration) error {
	return &psError{Class: "TimeoutExpired", Pid: pid, Msg: fmt.Sprintf("timeout after %v seconds", timeout.Seconds())}
}
