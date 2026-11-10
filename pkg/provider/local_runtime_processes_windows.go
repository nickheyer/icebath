//go:build windows

package provider

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	createSuspended     = 0x00000004
	jobKillOnJobClose   = 0x2000
	jobExtendedLimitCls = 9
)

// Kernel32 job calls behind seams so tests can inject real WinAPI rejections
var windowsJobAPI = struct {
	create func() (windows.Handle, error)
	set    func(job windows.Handle, class uint32, info uintptr, size uint32) error
	assign func(job, process windows.Handle) error
	close  func(h windows.Handle) error
}{
	create: func() (windows.Handle, error) { return windows.CreateJobObject(nil, nil) },
	set: func(job windows.Handle, class uint32, info uintptr, size uint32) error {
		_, err := windows.SetInformationJobObject(job, class, info, size)
		return err
	},
	assign: windows.AssignProcessToJobObject,
	close:  windows.CloseHandle,
}

var ntResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

// Resumes every thread of a process started suspended
var resumeProcessFn = func(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_SUSPEND_RESUME, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if status, _, _ := ntResumeProcess.Call(uintptr(h)); status != 0 {
		return windows.NTStatus(status)
	}
	return nil
}

// WindowsJob is the owner held kill on close job object that contains a router tree
type WindowsJob struct {
	mu     sync.Mutex
	handle windows.Handle
	open   bool
}

// Creates an unnamed non inheritable job that kills its tree on close and allows no breakaway
var newWindowsJob = func() (*WindowsJob, error) {
	h, err := windowsJobAPI.create()
	if err != nil {
		return nil, err
	}
	job := &WindowsJob{handle: h, open: true}
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limits.BasicLimitInformation.LimitFlags = jobKillOnJobClose
	if err := windowsJobAPI.set(h, jobExtendedLimitCls, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return nil, errors.Join(err, job.Close())
	}
	return job, nil
}

// Places the suspended process into the job through a handle opened while the pid is still held
var assignWindowsJob = func(job *WindowsJob, cmd *exec.Cmd) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windowsJobAPI.assign(job.handle, h)
}

// Terminates the contained tree, repeated closes are harmless
func (j *WindowsJob) Close() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.open {
		return nil
	}
	if err := windowsJobAPI.close(j.handle); err != nil {
		return err
	}
	j.open = false
	return nil
}

// Starts cmd suspended inside a fresh job then resumes it, a failed setup kills the child and frees every handle
func SpawnServer(ctx context.Context, cmd *exec.Cmd, owned bool) (*WindowsJob, error) {
	job, err := newWindowsJob()
	if err != nil {
		return nil, err
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createSuspended
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(err, job.Close())
	}
	err = assignWindowsJob(job, cmd)
	if err == nil {
		err = resumeProcessFn(cmd.Process.Pid)
	}
	if err == nil {
		return job, nil
	}
	return nil, errors.Join(err, abandonSuspendedChild(cmd), job.Close())
}

// Kills a child whose containment failed, waits up to ten seconds, and releases its pipes and handle
func abandonSuspendedChild(cmd *exec.Cmd) error {
	_ = cmd.Process.Kill()
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Release()
		return fmt.Errorf("timed out after 10 seconds waiting for pid %d to exit", cmd.Process.Pid)
	}
}
