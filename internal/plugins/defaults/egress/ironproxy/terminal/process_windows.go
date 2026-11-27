//go:build windows

package terminal

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Opens the path, refusing a reparse point planted there
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, &os.PathError{Op: "open", Path: path, Err: syscall.ELOOP}
	}
	return os.OpenFile(path, flag, perm)
}

// Owner of the open file as its SID, and whether it is this user or the owner this process gives new files
func owner(f *os.File) (string, bool, error) {
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return "", false, err
	}
	sid, _, err := sd.Owner()
	if err != nil {
		return "", false, err
	}
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", false, err
	}
	if sid.Equals(user.User.Sid) {
		return sid.String(), true, nil
	}
	fallback, err := defaultOwner(token)
	if err != nil {
		return "", false, err
	}
	return sid.String(), sid.Equals(fallback), nil
}

// Owner CreateFile gives the files this process makes
func defaultOwner(token windows.Token) (*windows.SID, error) {
	n := uint32(64)
	for {
		buf := make([]byte, n)
		err := windows.GetTokenInformation(token, windows.TokenOwner, &buf[0], n, &n)
		if err == nil {
			return (*struct{ Owner *windows.SID })(unsafe.Pointer(&buf[0])).Owner.Copy()
		}
		if err != windows.ERROR_INSUFFICIENT_BUFFER || n <= uint32(len(buf)) {
			return nil, err
		}
	}
}

// Leaves the daemon in the console group of the process that starts it
func detach(*exec.Cmd) {}

func terminate(p *os.Process) error {
	return p.Kill()
}

// Whether a process of the pid is running
func exists(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == 259
}

// Ends the process: Windows has no terminate a process can catch, so both are a kill
func signal(pid int, _ bool) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// Whether a signal failed for the process being gone
func gone(err error) bool {
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, windows.ERROR_INVALID_PARAMETER)
}
