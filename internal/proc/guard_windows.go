package proc

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Owner death coverage could not be arranged, so the child runs without dying with this process
var ErrUnguarded = errors.New("owner death coverage unavailable")

// Owner death coverage for child process trees, each held in a kill on close job only this process holds
type Guard struct {
	mu   sync.Mutex
	jobs map[int]windows.Handle
}

// Limits of an owner death job, its members free to leave on purpose with CREATE_BREAKAWAY_FROM_JOB
const guardJobFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK

// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION, whose active process count tells an emptied tree
type jobAccounting struct {
	TotalUserTime, TotalKernelTime, ThisPeriodTotalUserTime, ThisPeriodTotalKernelTime int64
	TotalPageFaultCount, TotalProcesses, ActiveProcesses, TotalTerminatedProcesses     uint32
}

// Empty guard
func NewGuard() *Guard { return &Guard{jobs: map[int]windows.Handle{}} }

// Starts cmd suspended inside a new kill on close job and resumes it, ErrUnguarded leaving the started child uncovered
func (g *Guard) Start(cmd *exec.Cmd) error {
	job, jobErr := newJob(guardJobFlags)
	if jobErr != nil {
		if err := cmd.Start(); err != nil {
			return err
		}
		return fmt.Errorf("%w: %w", ErrUnguarded, jobErr)
	}
	attrs(cmd).CreationFlags |= windows.CREATE_SUSPENDED
	if err := cmd.Start(); err != nil {
		windows.CloseHandle(job)
		return err
	}
	if err := assignAndResume(job, cmd.Process); err != nil {
		windows.CloseHandle(job)
		if rerr := resumeThreads(cmd.Process.Pid); rerr != nil {
			return errors.Join(err, rerr, cmd.Process.Kill())
		}
		return fmt.Errorf("%w: %w", ErrUnguarded, err)
	}
	g.mu.Lock()
	g.jobs[cmd.Process.Pid] = job
	g.mu.Unlock()
	return nil
}

// Places running incarnations into kill on close jobs, their later children joining them
func (g *Guard) Own(ids ...Identity) error {
	var failed []error
	for _, id := range ids {
		if err := g.own(id); err != nil {
			failed = append(failed, err)
		}
	}
	return errors.Join(failed...)
}

func (g *Guard) own(id Identity) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.jobs[id.PID]; ok {
		return nil
	}
	h, err := openIdentity(id, windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnguarded, err)
	}
	defer windows.CloseHandle(h)
	job, err := newJob(guardJobFlags)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnguarded, err)
	}
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("%w: assign pid %d: %w", ErrUnguarded, id.PID, err)
	}
	g.jobs[id.PID] = job
	return nil
}

// Stops covering trees, clearing kill on close before the job handle closes so the trees keep running
func (g *Guard) Release(pids ...int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	var failed []error
	for _, pid := range pids {
		if err := g.releaseLocked(pid); err != nil {
			failed = append(failed, err)
		}
	}
	return errors.Join(failed...)
}

func (g *Guard) releaseLocked(pid int) error {
	job, ok := g.jobs[pid]
	if !ok {
		return nil
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return fmt.Errorf("release pid %d: %w", pid, err)
	}
	windows.CloseHandle(job)
	delete(g.jobs, pid)
	return nil
}

// Whether the tree owned under pid has no process left, releasing it when so
func (g *Guard) releaseIfEmpty(pid int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	job, ok := g.jobs[pid]
	if !ok {
		return true
	}
	var acct jobAccounting
	if err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&acct)), uint32(unsafe.Sizeof(acct)), nil); err != nil || acct.ActiveProcesses > 0 {
		return false
	}
	return g.releaseLocked(pid) == nil
}

// Releases the tree owned under pid once no process is left in it, waiting until ctx ends
func (g *Guard) ReleaseWhenEmpty(ctx context.Context, pid int) bool {
	for {
		if g.releaseIfEmpty(pid) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Pids the job holds, read with a list grown until the kernel reports every member
func jobPids(job windows.Handle) ([]uintptr, error) {
	head := 2 * unsafe.Sizeof(uint32(0))
	slots := 64
	for {
		buf := make([]uintptr, int(head/unsafe.Sizeof(uintptr(0)))+slots)
		err := windows.QueryInformationJobObject(job, windows.JobObjectBasicProcessIdList, uintptr(unsafe.Pointer(&buf[0])), uint32(len(buf))*uint32(unsafe.Sizeof(uintptr(0))), nil)
		assigned := int(*(*uint32)(unsafe.Pointer(&buf[0])))
		listed := int(*(*uint32)(unsafe.Add(unsafe.Pointer(&buf[0]), 4)))
		if errors.Is(err, windows.ERROR_MORE_DATA) || (err == nil && listed < assigned) {
			slots = max(2*slots, assigned+16)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("job members: %w", err)
		}
		ids := buf[head/unsafe.Sizeof(uintptr(0)):]
		return append([]uintptr(nil), ids[:min(listed, len(ids))]...), nil
	}
}

// Handles of every process the job holds, each confirmed a member so a reused pid is never asked to close
func jobHandles(job windows.Handle) ([]windows.Handle, error) {
	pids, err := jobPids(job)
	if err != nil {
		return nil, err
	}
	var out []windows.Handle
	for _, pid := range pids {
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
		if err != nil {
			continue
		}
		var inJob int32
		if r, _, _ := procIsProcessInJob.Call(uintptr(h), uintptr(job), uintptr(unsafe.Pointer(&inJob))); r == 0 || inJob == 0 {
			windows.CloseHandle(h)
			continue
		}
		out = append(out, h)
	}
	return out, nil
}

// Asks the tree's windowed members to close, true once the tree emptied within grace
func (g *Guard) closeGracefully(ctx context.Context, pid int, job windows.Handle, grace time.Duration) bool {
	handles, err := jobHandles(job)
	if err != nil {
		return false
	}
	defer func() {
		for _, h := range handles {
			windows.CloseHandle(h)
		}
	}()
	if !closeWindows(handles) {
		return false
	}
	wait, cancel := context.WithTimeout(ctx, grace)
	defer cancel()
	return g.ReleaseWhenEmpty(wait, pid)
}

// Ends an owned tree, WM_CLOSE first with grace when a member has a window, then the job, true once released
func (g *Guard) StopSurvivors(ctx context.Context, pid int, grace time.Duration) bool {
	if g.releaseIfEmpty(pid) {
		return true
	}
	g.mu.Lock()
	job, ok := g.jobs[pid]
	g.mu.Unlock()
	if !ok {
		return true
	}
	if grace > 0 && g.closeGracefully(ctx, pid, job, grace) {
		return true
	}
	g.mu.Lock()
	if g.jobs[pid] != job {
		g.mu.Unlock()
		return true
	}
	err := windows.TerminateJobObject(job, 1)
	g.mu.Unlock()
	if err != nil {
		return false
	}
	wait, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return g.ReleaseWhenEmpty(wait, pid)
}
