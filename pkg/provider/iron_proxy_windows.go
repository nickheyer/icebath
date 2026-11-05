//go:build windows

package provider

import (
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Opens the link itself and refuses any reparse point with ELOOP, truncating only after the check
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var access uint32 = windows.READ_CONTROL
	switch flag & (os.O_RDONLY | os.O_WRONLY | os.O_RDWR) {
	case os.O_RDONLY:
		access |= windows.GENERIC_READ
	case os.O_WRONLY:
		access |= windows.GENERIC_WRITE
	case os.O_RDWR:
		access |= windows.GENERIC_READ | windows.GENERIC_WRITE
	}
	if flag&os.O_APPEND != 0 {
		access &^= windows.GENERIC_WRITE
		access |= windows.FILE_APPEND_DATA | windows.FILE_WRITE_ATTRIBUTES | windows.SYNCHRONIZE
	}
	disposition := uint32(windows.OPEN_EXISTING)
	switch {
	case flag&(os.O_CREATE|os.O_EXCL) == os.O_CREATE|os.O_EXCL:
		disposition = windows.CREATE_NEW
	case flag&os.O_CREATE != 0:
		disposition = windows.OPEN_ALWAYS
	}
	attrs := uint32(windows.FILE_ATTRIBUTE_NORMAL)
	if flag&os.O_CREATE != 0 && perm&0o200 == 0 {
		attrs = windows.FILE_ATTRIBUTE_READONLY
	}
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
	h, err := windows.CreateFile(name, access, share, nil, disposition, attrs|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		windows.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: syscall.ELOOP}
	}
	f := os.NewFile(uintptr(h), path)
	if flag&os.O_TRUNC != 0 {
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

// Owner SID of the open file and whether it is the token user or the default owner this process stamps
func fdOwner(f *os.File) (string, bool, error) {
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return "", false, err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return "", false, err
	}
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", false, err
	}
	if owner.Equals(user.User.Sid) {
		return owner.String(), true, nil
	}
	defaultOwner, err := tokenDefaultOwner(token)
	if err != nil {
		return "", false, err
	}
	return owner.String(), owner.Equals(defaultOwner), nil
}

// Copy of the TokenOwner SID that CreateFile assigns as owner of new files
func tokenDefaultOwner(token windows.Token) (*windows.SID, error) {
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

// Leaves the daemon in the caller console group exactly like the Popen call that passes no Windows flags
func detachDaemon(*exec.Cmd) {}

// TerminateProcess with exit code 1 like Popen.terminate on Windows
func terminateChild(p *os.Process) error {
	return p.Kill()
}
