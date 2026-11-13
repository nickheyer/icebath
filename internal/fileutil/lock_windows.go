//go:build windows

package fileutil

import (
	"errors"
	"io/fs"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Exit code GetExitCodeProcess reports for a process that is still running
const stillActive = 259

// Span a wait without a deadline retries a held lock, matching ten attempts one second apart
const blockingLockBudget = 9 * time.Second

// Byte range lock over the configured range without waiting, contention mapped to ErrLocked
func kernelLock(f *os.File, cfg lockConfig) error {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if !cfg.shared {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	err := rangeOp(f, cfg, func(h windows.Handle, ol *windows.Overlapped) error {
		return windows.LockFileEx(h, flags, 0, uint32(cfg.length), uint32(cfg.length>>32), ol)
	})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return &fs.PathError{Op: "lock", Path: f.Name(), Err: ErrLocked}
	}
	return err
}

// Retries a held lock until the blocking budget runs out, then reports it as ErrLocked
func blockingLock(f *os.File, cfg lockConfig) error {
	deadline := time.Now().Add(blockingLockBudget)
	for {
		err := kernelLock(f, cfg)
		if !errors.Is(err, ErrLocked) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(lockPollInterval)
	}
}

// Unlocks the configured byte range of f
func unlockFile(f *os.File, cfg lockConfig) error {
	return rangeOp(f, cfg, func(h windows.Handle, ol *windows.Overlapped) error {
		return windows.UnlockFileEx(h, 0, uint32(cfg.length), uint32(cfg.length>>32), ol)
	})
}

// Runs op on the handle of f with an overlapped structure positioned at the configured offset
func rangeOp(f *os.File, cfg lockConfig, op func(windows.Handle, *windows.Overlapped) error) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var opErr error
	ctlErr := conn.Control(func(fd uintptr) {
		ol := windows.Overlapped{Offset: uint32(cfg.offset), OffsetHigh: uint32(cfg.offset >> 32)}
		opErr = op(windows.Handle(fd), &ol)
	})
	if ctlErr != nil {
		return ctlErr
	}
	if opErr != nil {
		return &fs.PathError{Op: "lock", Path: f.Name(), Err: opErr}
	}
	return nil
}

// Lock file opened read write and created with the configured mode, refusing a reparse point under NoFollow
func openLockFile(path string, cfg lockConfig) (*os.File, error) {
	if !cfg.noFollow {
		return os.OpenFile(path, os.O_RDWR|os.O_CREATE, cfg.perm)
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return nil, joinErr(&os.PathError{Op: "open", Path: path, Err: err}, windows.CloseHandle(h))
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return nil, joinErr(&os.PathError{Op: "open", Path: path, Err: ErrSymlink}, windows.CloseHandle(h))
	}
	return os.NewFile(uintptr(h), path), nil
}

// Reports whether a process with pid is still running, one this process may not query counting as alive
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}
