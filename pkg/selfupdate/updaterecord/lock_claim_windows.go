//go:build windows

package updaterecord

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const noFollow = 0

// Opens the sidecar with no sharing, retrying read only on access denied, MarkerBusy after wait
func mutexAcquire(path string, wait time.Duration) (func(), error) {
	deadline := time.Now().Add(wait)
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	for {
		var last error
		for _, mode := range [][2]uint32{{windows.GENERIC_READ | windows.GENERIC_WRITE, windows.OPEN_ALWAYS}, {windows.GENERIC_READ, windows.OPEN_EXISTING}} {
			handle, err := windows.CreateFile(name, mode[0], 0, nil, mode[1], windows.FILE_ATTRIBUTE_NORMAL, 0)
			if err == nil {
				return func() { windows.CloseHandle(handle) }, nil
			}
			last = err
			if errors.Is(err, windows.ERROR_SHARING_VIOLATION) || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				break
			}
		}
		if !errors.Is(last, windows.ERROR_SHARING_VIOLATION) {
			return nil, &os.PathError{Op: "CreateFile", Path: path, Err: last}
		}
		if !time.Now().Before(deadline) {
			return nil, &MarkerBusy{Path: path}
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Locks count bytes at offset exclusively without waiting
func lockBytes(f *os.File, offset int64, count uint32) bool {
	ol := windows.Overlapped{Offset: uint32(offset), OffsetHigh: uint32(offset >> 32)}
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, count, 0, &ol) == nil
}

func unlockBytes(f *os.File, offset int64, count uint32) {
	ol := windows.Overlapped{Offset: uint32(offset), OffsetHigh: uint32(offset >> 32)}
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, count, 0, &ol)
}

// Locks the owner byte and test locks every lease byte, false while any is held
func tryLock(f *os.File) (bool, error) {
	if !lockBytes(f, windowsLockOffset, 1) {
		return false, nil
	}
	if !lockBytes(f, windowsLockOffset+1, leaseSlots) {
		unlockBytes(f, windowsLockOffset, 1)
		return false, nil
	}
	unlockBytes(f, windowsLockOffset+1, leaseSlots)
	return true, nil
}

// Shared try over the owner byte and every lease byte, given straight back
func trySharedLock(f *os.File) (bool, error) {
	handle := windows.Handle(f.Fd())
	ol := windows.Overlapped{Offset: windowsLockOffset}
	if windows.LockFileEx(handle, windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1+leaseSlots, 0, &ol) != nil {
		return false, nil
	}
	ol = windows.Overlapped{Offset: windowsLockOffset}
	_ = windows.UnlockFileEx(handle, 0, 1+leaseSlots, 0, &ol)
	return true, nil
}

// Locks a free lease byte, false when the tree's other joiners hold every one
func takeLease(f *os.File) (int64, bool) {
	for offset := int64(windowsLockOffset + 1); offset < windowsLockOffset+1+leaseSlots; offset++ {
		if lockBytes(f, offset, 1) {
			return offset, true
		}
	}
	return 0, false
}

func inheritedLockFD(path string) (int, bool) { return -1, false }

// Hard link count of an open file
func linkCount(f *os.File) (uint64, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return 0, err
	}
	return uint64(info.NumberOfLinks), nil
}

var treeJob struct {
	sync.Mutex
	handle windows.Handle
}

// Kill on close job of this process for the update tree, created once and never closed
func UpdateTreeJob() (windows.Handle, error) {
	treeJob.Lock()
	defer treeJob.Unlock()
	if treeJob.handle != 0 {
		return treeJob.handle, nil
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	treeJob.handle = job
	return job, nil
}

// Puts an update tree child in the kill on close job, the refusal logged and returned when it cannot
func BindChildToUpdateTree(pid int) error {
	err := bindToKillOnCloseJob(pid)
	if err != nil {
		slog.Warn("Could not bind update child to the update's job", "pid", pid, "error", err)
	}
	return err
}

func bindToKillOnCloseJob(pid int) error {
	job, err := UpdateTreeJob()
	if err != nil {
		return err
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	return windows.AssignProcessToJobObject(job, process)
}

var ntResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

// Resumes a CreateSuspended child, killing it when the resume fails
func ResumeSuspendedChild(process *os.Process) error {
	handle, err := windows.OpenProcess(0x0800, false, uint32(process.Pid))
	if err == nil {
		status, _, _ := ntResumeProcess.Call(uintptr(handle))
		windows.CloseHandle(handle)
		if status == 0 {
			return nil
		}
	}
	_ = process.Kill()
	return fmt.Errorf("could not resume update child %d", process.Pid)
}
