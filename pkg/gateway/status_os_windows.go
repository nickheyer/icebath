//go:build windows

package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

const (
	oNoFollow = 0
	oNonblock = 0
)

// Runs taskkill as a tree force kill with a hidden console and a ten second timeout
var statusTaskkill = func(pid int) (int, string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "taskkill", "/PID", fmt.Sprint(pid), "/T", "/F")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if errors.Is(err, exec.ErrNotFound) {
		return 0, "", "", fmt.Errorf("taskkill: %w", fs.ErrNotExist)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), stdout.String(), stderr.String(), nil
	}
	if err != nil {
		return 0, "", "", err
	}
	return 0, stdout.String(), stderr.String(), nil
}

// Locks one byte at the high offset so readers of the record are never blocked, seeding an empty file
func tryAcquireFileLock(f *os.File) bool {
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return false
	}
	if end == 0 {
		if _, err := f.WriteString("\n"); err != nil {
			return false
		}
		if err := f.Sync(); err != nil {
			return false
		}
	}
	ol := &windows.Overlapped{Offset: windowsLockOffset}
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol) == nil
}

func releaseFileLock(f *os.File) {
	ol := &windows.Overlapped{Offset: windowsLockOffset}
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}

// Terminates pid with exit code 15 like os.kill with SIGTERM, the only signal Windows offers either way
func killPid(pid int, _ bool) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return &os.SyscallError{Syscall: "OpenProcess", Err: err}
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 15); err != nil {
		return &os.SyscallError{Syscall: "TerminateProcess", Err: err}
	}
	return nil
}

// Refuses a lock path that is not a regular file
func tightenLockMode(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return &fs.PathError{Op: "open", Path: f.Name(), Err: fmt.Errorf("unsafe gateway lock owner or type: %w", fs.ErrPermission)}
	}
	return nil
}

// A sharing violation is a live holder on Windows, so no stale lock is ever unlinked
func unlinkStaleLock(path string) (bool, error) { return false, nil }

func stillNames(path string, f *os.File) bool { return true }
