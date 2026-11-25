//go:build windows

package treekill

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Job objects are the Windows process trees, one job per tree with its members listed and ended by the kernel

// Limit flags a job is created or changed with
const (
	KillOnClose = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	BreakawayOK = windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
)

var (
	procIsProcessInJob  = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")
	procNtResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")
)

// CreateJob makes an unnamed job object no child inherits a handle to, with the given limit flags
func CreateJob(limits uint32) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	if limits != 0 {
		if err := SetLimits(job, limits); err != nil {
			return 0, errors.Join(err, windows.CloseHandle(job))
		}
	}
	return job, nil
}

// SetLimits replaces the basic limit flags of a job
func SetLimits(job windows.Handle, limits uint32) error {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = limits
	_, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	return err
}

// AssignHandle places an open process in a job
func AssignHandle(job, process windows.Handle) error {
	return windows.AssignProcessToJobObject(job, process)
}

// AssignPid places the process pid in a job
func AssignPid(job windows.Handle, pid int) error {
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	return windows.AssignProcessToJobObject(job, process)
}

// Resume runs every thread of a process created suspended
func Resume(process windows.Handle) error {
	if status, _, _ := procNtResumeProcess.Call(uintptr(process)); status != 0 {
		return windows.NTStatus(status)
	}
	return nil
}

// ResumePid runs every thread of the suspended process pid
func ResumePid(pid int) error {
	process, err := windows.OpenProcess(windows.PROCESS_SUSPEND_RESUME, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	return Resume(process)
}

// InJob reports whether process is a member of job, a zero job asking about any job
func InJob(process, job windows.Handle) (bool, error) {
	var inside int32
	if ok, _, err := procIsProcessInJob.Call(uintptr(process), uintptr(job), uintptr(unsafe.Pointer(&inside))); ok == 0 {
		return false, err
	}
	return inside != 0, nil
}

// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION, ActiveProcesses counting the live members
type basicAccounting struct {
	TotalUserTime, TotalKernelTime, ThisPeriodTotalUserTime, ThisPeriodTotalKernelTime int64
	TotalPageFaultCount, TotalProcesses, ActiveProcesses, TotalTerminatedProcesses     uint32
}

// ActiveProcesses counts the live members of a job
func ActiveProcesses(job windows.Handle) (uint32, error) {
	var usage basicAccounting
	err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&usage)), uint32(unsafe.Sizeof(usage)), nil)
	return usage.ActiveProcesses, err
}

// Pids lists the live members of a job from its JOBOBJECT_BASIC_PROCESS_ID_LIST
func Pids(job windows.Handle) ([]int, error) {
	word := unsafe.Sizeof(uintptr(0))
	header := 8 / word
	for room := uintptr(64); ; {
		buf := make([]uintptr, header+room)
		err := windows.QueryInformationJobObject(job, windows.JobObjectBasicProcessIdList, uintptr(unsafe.Pointer(&buf[0])), uint32(uintptr(len(buf))*word), nil)
		counts := (*[2]uint32)(unsafe.Pointer(&buf[0]))
		if errors.Is(err, windows.ERROR_MORE_DATA) {
			room = max(room*2, uintptr(counts[0])+16)
			continue
		}
		if err != nil {
			return nil, err
		}
		out := make([]int, 0, counts[1])
		for _, id := range buf[header : header+uintptr(counts[1])] {
			out = append(out, int(id))
		}
		return out, nil
	}
}

// TerminateJob ends every member of a job with exit code code
func TerminateJob(job windows.Handle, code uint32) error {
	return windows.TerminateJobObject(job, code)
}

// Job is one job object holding a process tree, its handle closed at most once
type Job struct {
	mu     sync.Mutex
	handle windows.Handle
	limits uint32
}

// NewJob creates a job with the given limit flags
func NewJob(limits uint32) (*Job, error) {
	handle, err := CreateJob(limits)
	if err != nil {
		return nil, err
	}
	return &Job{handle: handle, limits: limits}, nil
}

// Runs fn on the job handle while it is open, os.ErrClosed after Close
func (j *Job) with(fn func(windows.Handle) error) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.handle == 0 {
		return os.ErrClosed
	}
	return fn(j.handle)
}

// Handle is the job object handle, zero once closed
func (j *Job) Handle() windows.Handle {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.handle
}

// Open reports whether the job handle is still held
func (j *Job) Open() bool { return j.Handle() != 0 }

// SetKillOnClose decides whether closing the job, this process dying included, ends every member
func (j *Job) SetKillOnClose(on bool) error {
	return j.with(func(h windows.Handle) error {
		limits := j.limits &^ KillOnClose
		if on {
			limits |= KillOnClose
		}
		if limits == j.limits {
			return nil
		}
		if err := SetLimits(h, limits); err != nil {
			return err
		}
		j.limits = limits
		return nil
	})
}

// Assign places the process pid in the job
func (j *Job) Assign(pid int) error {
	return j.with(func(h windows.Handle) error { return AssignPid(h, pid) })
}

// AssignHandle places an open process in the job
func (j *Job) AssignHandle(process windows.Handle) error {
	return j.with(func(h windows.Handle) error { return AssignHandle(h, process) })
}

// Active counts the live members of the job
func (j *Job) Active() (uint32, error) {
	var n uint32
	err := j.with(func(h windows.Handle) (err error) {
		n, err = ActiveProcesses(h)
		return err
	})
	return n, err
}

// Pids lists the live members of the job
func (j *Job) Pids() ([]int, error) {
	var out []int
	err := j.with(func(h windows.Handle) (err error) {
		out, err = Pids(h)
		return err
	})
	return out, err
}

// Terminate ends every member of the job with exit code code
func (j *Job) Terminate(code uint32) error {
	return j.with(func(h windows.Handle) error { return TerminateJob(h, code) })
}

// Close releases the job handle, ending every member when kill on close is set, repeated closes are harmless
func (j *Job) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.handle == 0 {
		return nil
	}
	if err := windows.CloseHandle(j.handle); err != nil {
		return err
	}
	j.handle = 0
	return nil
}

// StartSuspended starts cmd with its threads held, runs enter and then resume on its pid, a failed step killing and reaping the child
func StartSuspended(cmd *exec.Cmd, enter, resume func(pid int) error) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	if err := cmd.Start(); err != nil {
		return err
	}
	err := enter(cmd.Process.Pid)
	if err == nil {
		err = resume(cmd.Process.Pid)
	}
	if err != nil {
		return errors.Join(err, abandonChild(cmd.Process, func() error { return cmd.Wait() }))
	}
	return nil
}

// StartInJob starts cmd inside job before it runs a single instruction
func StartInJob(cmd *exec.Cmd, job *Job) error {
	return StartSuspended(cmd, job.Assign, ResumePid)
}

// Kills a child whose setup failed, waits up to ten seconds for its reap, and releases its handle when it never comes
func abandonChild(process *os.Process, reap func() error) error {
	_ = process.Kill()
	done := make(chan struct{})
	go func() {
		_ = reap()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(10 * time.Second):
		_ = process.Release()
		return fmt.Errorf("timed out after 10 seconds waiting for pid %d to exit", process.Pid)
	}
}

// Jobs of the trees this process started or adopted, keyed by the pid of the tree's first process
var trees = struct {
	sync.Mutex
	jobs map[int]*Job
}{jobs: map[int]*Job{}}

// Track records job as the tree pid leads and closes the jobs of tracked trees that ended
func Track(pid int, job *Job) {
	trees.Lock()
	ended := pruneLocked()
	if old := trees.jobs[pid]; old != nil && old != job {
		ended = append(ended, old)
	}
	trees.jobs[pid] = job
	trees.Unlock()
	closeJobs(ended)
}

// Tracked is the job of the tree pid leads, nil when no such tree is tracked
func Tracked(pid int) *Job {
	trees.Lock()
	defer trees.Unlock()
	return trees.jobs[pid]
}

// Prune closes the jobs of every tracked tree with no member left
func Prune() {
	trees.Lock()
	ended := pruneLocked()
	trees.Unlock()
	closeJobs(ended)
}

// Drops tracked trees with no live member and returns their jobs, caller holds the lock
func pruneLocked() []*Job {
	var ended []*Job
	for pid, job := range trees.jobs {
		if n, err := job.Active(); errors.Is(err, os.ErrClosed) || err == nil && n == 0 {
			delete(trees.jobs, pid)
			ended = append(ended, job)
		}
	}
	return ended
}

func closeJobs(jobs []*Job) {
	for _, job := range jobs {
		if err := job.Close(); err != nil {
			pkgLog().Debug("closing the job of an ended process tree failed", "error", err)
		}
	}
}

// Adopt puts the running process pid and the descendants it already has in a fresh tracked job
func Adopt(pid int) (*Job, error) {
	birth, err := processBirth(pid)
	if err != nil {
		return nil, err
	}
	job, err := NewJob(0)
	if err != nil {
		return nil, err
	}
	if err := job.Assign(pid); err != nil {
		return nil, errors.Join(err, job.Close())
	}
	for _, child := range descendants(pid, birth) {
		if err := job.Assign(child); err != nil {
			pkgLog().Debug(fmt.Sprintf("adopting descendant %d of %d failed", child, pid), "error", err)
		}
	}
	Track(pid, job)
	return job, nil
}

// LeaderOf names the tracked tree holding pid, ErrProcessGone when no tracked tree holds it
func LeaderOf(pid int) (int, error) {
	trees.Lock()
	jobs := make(map[int]*Job, len(trees.jobs))
	for leader, job := range trees.jobs {
		jobs[leader] = job
	}
	trees.Unlock()
	if job := jobs[pid]; job != nil {
		if n, err := job.Active(); err == nil && n > 0 {
			return pid, nil
		}
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, ErrProcessGone
	}
	defer windows.CloseHandle(process)
	for leader, job := range jobs {
		handle := job.Handle()
		if handle == 0 {
			continue
		}
		if inside, err := InJob(process, handle); err == nil && inside {
			return leader, nil
		}
	}
	return 0, ErrProcessGone
}

// TreeGone reports whether no member of the tree pid leads is left, an untracked pid answering for itself
func TreeGone(pid int) bool {
	if job := Tracked(pid); job != nil {
		n, err := job.Active()
		return errors.Is(err, os.ErrClosed) || err == nil && n == 0
	}
	return !processRunning(pid)
}

// TerminateTree ends every member of the tracked tree pid leads, ErrProcessGone when it has none left or is not tracked
func TerminateTree(pid int, code uint32) error {
	job := Tracked(pid)
	if job == nil {
		return ErrProcessGone
	}
	if n, err := job.Active(); errors.Is(err, os.ErrClosed) || err == nil && n == 0 {
		return ErrProcessGone
	}
	return job.Terminate(code)
}

// Members lists the live processes of the tracked tree pid leads, ErrProcessGone when it is not tracked
func Members(pid int) ([]int, error) {
	job := Tracked(pid)
	if job == nil {
		return nil, ErrProcessGone
	}
	return job.Pids()
}
