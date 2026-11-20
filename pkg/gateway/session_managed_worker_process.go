package gateway

import (
	"os"
	"os/exec"
	"sync/atomic"
	"time"
)

// Hidden first argument a main or TestMain routes to the managed worker child before any other parsing
const ManagedWorkerEntry = "__icebath_managed_worker__"

// Latch proving this binary dispatches the managed worker entry, the only state in which the owner may spawn one
var managedWorkerEntryDispatched atomic.Bool

// Records that this binary routes the managed worker entry to its child main
func MarkManagedWorkerEntryDispatched() { managedWorkerEntryDispatched.Store(true) }

// Managed worker child with the owner's pipe ends, reaped in the background like a polled Popen
type workerProcess struct {
	cmd    *exec.Cmd
	Pid    int
	stdin  *os.File
	stdout *os.File
	stderr *os.File
	exited chan struct{}
}

// Whether the child exited, the returncode of a Popen poll being set
func (p *workerProcess) poll() bool {
	select {
	case <-p.exited:
		return true
	default:
		return false
	}
}

// Waits for the child to exit within seconds, a negative timeout waiting forever, false when it timed out
func (p *workerProcess) wait(seconds float64) bool {
	if seconds < 0 {
		<-p.exited
		return true
	}
	timer := time.NewTimer(time.Duration(seconds * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-p.exited:
		return true
	case <-timer.C:
		return false
	}
}

// Starts this binary as the managed worker child, stderr piped only for a log sink
func spawnManagedWorker(env []string, cwd string, logStderr bool) (*workerProcess, error) {
	if !managedWorkerEntryDispatched.Load() {
		panic("a managed worker would re-execute a binary that never dispatches gateway.ManagedWorkerEntry, call its dispatcher first in main or TestMain")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	var parent, child []*os.File
	closeAll := func(files []*os.File) {
		for _, f := range files {
			if f != nil {
				f.Close()
			}
		}
	}
	pipes := 2
	if logStderr {
		pipes = 3
	}
	for i := 0; i < pipes; i++ {
		r, w, err := os.Pipe()
		if err != nil {
			closeAll(parent)
			closeAll(child)
			return nil, err
		}
		if i == 0 {
			parent, child = append(parent, w), append(child, r)
		} else {
			parent, child = append(parent, r), append(child, w)
		}
	}
	cmd := exec.Command(exe, ManagedWorkerEntry)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Stdin, cmd.Stdout = child[0], child[1]
	if logStderr {
		cmd.Stderr = child[2]
	}
	cmd.SysProcAttr = managedWorkerAttr()
	err = cmd.Start()
	closeAll(child)
	if err != nil {
		closeAll(parent)
		return nil, err
	}
	p := &workerProcess{cmd: cmd, Pid: cmd.Process.Pid, stdin: parent[0], stdout: parent[1], exited: make(chan struct{})}
	if logStderr {
		p.stderr = parent[2]
	}
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}
