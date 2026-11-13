//go:build windows

package fileutil

import (
	"errors"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Random names a private temporary file tries before giving up on a crowded directory
const privateTempAttempts = 10000

// New leaf directory under a protected inheriting DACL for SYSTEM, Administrators, the owner and this user, an existing one kept
func mkdirPrivate(path string) error {
	user, err := currentUserSID()
	if err != nil {
		return &os.PathError{Op: "mkdir", Path: path, Err: err}
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;OW)(A;OICI;FA;;;" + user + ")")
	if err != nil {
		return &os.PathError{Op: "mkdir", Path: path, Err: err}
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return &os.PathError{Op: "mkdir", Path: path, Err: err}
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	err = windows.CreateDirectory(p, sa)
	if err == nil {
		return nil
	}
	if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return &os.PathError{Op: "mkdir", Path: path, Err: err}
	}
	fi, statErr := os.Stat(path)
	if statErr != nil {
		return statErr
	}
	if !fi.IsDir() {
		return &os.PathError{Op: "mkdir", Path: path, Err: syscall.ENOTDIR}
	}
	return nil
}

// Protected DACL granting full control to user, SYSTEM and Administrators only, the descriptor every private file gets
func privateFileDACL(user string) string { return "D:P(A;;FA;;;" + user + ")(A;;FA;;;SY)(A;;FA;;;BA)" }

// Replaces the DACL of path with the protected one a private temporary file is created with
func protectPrivateFile(path string) error {
	user, err := currentUserSID()
	if err != nil {
		return &os.PathError{Op: "protect", Path: path, Err: err}
	}
	sd, err := windows.SecurityDescriptorFromString(privateFileDACL(user))
	if err != nil {
		return &os.PathError{Op: "protect", Path: path, Err: err}
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return &os.PathError{Op: "protect", Path: path, Err: err}
	}
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	if err != nil {
		return &os.PathError{Op: "protect", Path: path, Err: err}
	}
	return nil
}

// Temporary file created under the protected private DACL, owned by the user the process runs as
func createPrivateTemp(dir, prefix, suffix string) (*os.File, error) {
	user, err := currentUserSID()
	if err != nil {
		return nil, &os.PathError{Op: "createtemp", Path: dir, Err: err}
	}
	sd, err := windows.SecurityDescriptorFromString("O:" + user + privateFileDACL(user))
	if err != nil {
		return nil, &os.PathError{Op: "createtemp", Path: dir, Err: err}
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	for range privateTempAttempts {
		name := filepath.Join(dir, prefix+strconv.FormatUint(uint64(rand.Uint32()), 10)+suffix)
		p, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return nil, &os.PathError{Op: "createtemp", Path: name, Err: err}
		}
		h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, sa,
			windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			continue
		}
		if err != nil {
			return nil, &os.PathError{Op: "createtemp", Path: name, Err: err}
		}
		return os.NewFile(uintptr(h), name), nil
	}
	return nil, &os.PathError{Op: "createtemp", Path: filepath.Join(dir, prefix+"*"+suffix), Err: fs.ErrExist}
}

// String SID of the user the process token runs as
func currentUserSID() (string, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return tu.User.Sid.String(), nil
}
