package home

import (
	"io/fs"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Protected DACL a private directory is created with, SYSTEM, Administrators and the owner with inheriting full control
const privateDirDACL = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;OW)"

// Creates the single directory path, 0700 giving it the protected owner DACL and other modes inheriting the parent
func mkdirLeaf(path string, perm fs.FileMode) error {
	if perm.Perm() != 0o700 {
		return os.Mkdir(path, perm)
	}
	sd, err := windows.SecurityDescriptorFromString(privateDirDACL)
	if err != nil {
		return &fs.PathError{Op: "mkdir", Path: path, Err: err}
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return &fs.PathError{Op: "mkdir", Path: path, Err: err}
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	if err := windows.CreateDirectory(p, sa); err != nil {
		return &fs.PathError{Op: "mkdir", Path: path, Err: err}
	}
	return nil
}

// Windows has no umask, so a managed home keeps the modes its ACLs give
func setUmask(int) int { return 0 }

// Windows has no umask to put back
func restoreUmask(int) {}
