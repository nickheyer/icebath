//go:build windows

package gateway

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"golang.org/x/sys/windows"
)

// Opens path with the private security attributes and refuses an escaped, reparse or permissive handle
func sshOpenVerified(path string, access, creation, flags, share uint32) (sshHandle, error) {
	sa, owner, err := privateSecurityAttributes()
	if err != nil {
		return 0, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	handle, err := windows.CreateFile(name, access, share, sa, creation, flags, 0)
	if err != nil {
		return 0, err
	}
	verify := func() error {
		buf := make([]uint16, windows.MAX_LONG_PATH)
		n, err := windows.GetFinalPathNameByHandle(handle, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return err
		}
		actual := strings.TrimPrefix(windows.UTF16ToString(buf[:n]), `\\?\`)
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if pypath.Normcase(actual) != pypath.Normcase(abs) {
			return errors.New("Windows SSH runtime handle escaped its expected path")
		}
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
			return err
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return errors.New("Windows SSH runtime path contains a reparse point")
		}
		return verifyPrivateSecurity(handle, owner)
	}
	if err := verify(); err != nil {
		windows.CloseHandle(handle)
		return 0, err
	}
	return sshHandle(handle), nil
}

// Starts args detached from the console, the process group and the job with both outputs on the log handle
func sshSpawnDetached(args, env []string, log sshHandle, logPath string) (int, error) {
	logFile := os.NewFile(uintptr(log), logPath)
	defer logFile.Close()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_BREAKAWAY_FROM_JOB}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	return pid, cmd.Process.Release()
}

// Win32 calls of the runtime bound to the real system
func newSSHWin32() *sshWin32 {
	return &sshWin32{
		available: func() error { return nil },
		open:      sshOpenVerified,
		read: func(h sshHandle, limit int) ([]byte, error) {
			buf := make([]byte, limit)
			var n uint32
			if err := windows.ReadFile(windows.Handle(h), buf, &n, nil); err != nil && !errors.Is(err, windows.ERROR_HANDLE_EOF) {
				return nil, err
			}
			return buf[:n], nil
		},
		write: func(h sshHandle, data []byte) error {
			var written uint32
			return windows.WriteFile(windows.Handle(h), data, &written, nil)
		},
		flush: func(h sshHandle) error { return windows.FlushFileBuffers(windows.Handle(h)) },
		close: func(h sshHandle) { windows.CloseHandle(windows.Handle(h)) },
		createDirectory: func(path string) error {
			sa, _, err := privateSecurityAttributes()
			if err != nil {
				return err
			}
			name, err := windows.UTF16PtrFromString(path)
			if err != nil {
				return err
			}
			return windows.CreateDirectory(name, sa)
		},
		moveFileEx: func(src, dst string, flags uint32) error {
			from, err := windows.UTF16PtrFromString(src)
			if err != nil {
				return err
			}
			to, err := windows.UTF16PtrFromString(dst)
			if err != nil {
				return err
			}
			return windows.MoveFileEx(from, to, flags)
		},
		winerror: func(err error) (int, bool) {
			var errno syscall.Errno
			if !errors.As(err, &errno) {
				return 0, false
			}
			return int(errno), true
		},
		spawnDetached: sshSpawnDetached,
	}
}
