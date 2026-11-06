package selfupdate

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Adds CREATE_SUSPENDED so the child runs no instruction before it is bound to the update job
func suspendedAttr(attr *syscall.SysProcAttr) *syscall.SysProcAttr {
	out := &syscall.SysProcAttr{}
	if attr != nil {
		copied := *attr
		out = &copied
	}
	out.CreationFlags |= windows.CREATE_SUSPENDED
	return out
}

// Process attributes that hand exactly this handle to the launcher through its inherited handle list
func inheritHandleAttr(handle uintptr) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(handle)}}
}

// Inheritable duplicate of the update's kill on close job handle with the call that closes it
func inheritableJobHandle() (uintptr, func(), error) {
	job, err := updaterecord.UpdateTreeJob()
	if err == nil {
		me := windows.CurrentProcess()
		var dup windows.Handle
		if err = windows.DuplicateHandle(me, job, me, &dup, 0, true, windows.DUPLICATE_SAME_ACCESS); err == nil {
			return uintptr(dup), func() { _ = windows.CloseHandle(dup) }, nil
		}
	}
	slog.Warn("Could not hand the update's job to a build child: " + err.Error())
	return 0, nil, err
}

// The update job owns git and every helper it starts on Windows, so the job bound runner is the owner watch
func runOwnerWatched(ctx context.Context, argv []string, opts ChildOptions, watchOwner bool) (*CompletedProcess, error) {
	return Run(ctx, argv, !watchOwner, opts)
}

type winJoinAPI struct{ lastErr error }

func newJoinAPI() joinAPI { return &winJoinAPI{} }

func (a *winJoinAPI) note(err error) bool {
	a.lastErr = err
	return err == nil
}

func (a *winJoinAPI) Assign(job, process uintptr) bool {
	return a.note(windows.AssignProcessToJobObject(windows.Handle(job), windows.Handle(process)))
}

func (a *winJoinAPI) CurrentProcess() uintptr { return uintptr(windows.CurrentProcess()) }

func (a *winJoinAPI) LastError() int {
	var errno syscall.Errno
	if errors.As(a.lastErr, &errno) {
		return int(errno)
	}
	return 0
}

func (a *winJoinAPI) CreateJob() uintptr {
	job, err := windows.CreateJobObject(nil, nil)
	a.note(err)
	return uintptr(job)
}

func (a *winJoinAPI) SetKillOnClose(job uintptr) bool {
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err := windows.SetInformationJobObject(windows.Handle(job), windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)))
	return a.note(err)
}

func (a *winJoinAPI) InJob(process, job uintptr) bool {
	var inside int32
	ok, _, err := procIsProcessInJob.Call(process, job, uintptr(unsafe.Pointer(&inside)))
	if ok == 0 {
		a.lastErr = err
		return false
	}
	return inside != 0
}

func (a *winJoinAPI) CloseHandle(handle uintptr) { _ = windows.CloseHandle(windows.Handle(handle)) }

var procIsProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

var procNtResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

func (a *winJoinAPI) Resume(process uintptr) int {
	status, _, _ := procNtResumeProcess.Call(process)
	return int(int32(status))
}

func (a *winJoinAPI) Terminate(job uintptr, code uint32) {
	_ = windows.TerminateJobObject(windows.Handle(job), code)
}

// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION whose ActiveProcesses says when the command's tree is gone
type jobAccounting struct {
	TotalUserTime, TotalKernelTime, ThisPeriodTotalUserTime, ThisPeriodTotalKernelTime int64
	TotalPageFaultCount, TotalProcesses, ActiveProcesses, TotalTerminatedProcesses     uint32
}

func (a *winJoinAPI) ActiveProcesses(job uintptr) (uint32, bool) {
	var usage jobAccounting
	err := windows.QueryInformationJobObject(windows.Handle(job), windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&usage)), uint32(unsafe.Sizeof(usage)), nil)
	return usage.ActiveProcesses, a.note(err)
}

func (a *winJoinAPI) Stderr() io.Writer { return os.Stderr }

type winJoinChild struct {
	cmd    *exec.Cmd
	handle windows.Handle
}

func (a *winJoinAPI) StartSuspended(argv []string) (joinChild, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_SUSPEND_RESUME|windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	return &winJoinChild{cmd: cmd, handle: handle}, nil
}

func (c *winJoinChild) Handle() uintptr { return uintptr(c.handle) }

func (c *winJoinChild) Wait() int {
	_ = c.cmd.Wait()
	return c.cmd.ProcessState.ExitCode()
}

func (c *winJoinChild) Kill() { _ = c.cmd.Process.Kill() }
