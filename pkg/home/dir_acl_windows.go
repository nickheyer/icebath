package home

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	tokenQuery                     = 0x0008
	tokenElevation                 = 20
	readControl                    = 0x00020000
	writeDAC                       = 0x00040000
	fileShareAll                   = 0x7
	openExisting                   = 3
	fileFlagBackupSemantics        = 0x02000000
	fileFlagOpenReparsePoint       = 0x00200000
	processQueryLimitedInformation = 0x1000
	fileAllAccess                  = 0x1F01FF
	accessAllowedACEType           = 0
	objectInheritACE               = 0x1
	containerInheritACE            = 0x2
	inheritOnlyACE                 = 0x8
	aclRevision                    = 2
	aclRevisionDS                  = 4
	accessAllowedACESize           = 12
)

var (
	advapi32                    = windows.NewLazySystemDLL("advapi32.dll")
	procGetKernelObjectSecurity = advapi32.NewProc("GetKernelObjectSecurity")
	procAddAccessAllowedAceEx   = advapi32.NewProc("AddAccessAllowedAceEx")
	procInitializeAcl           = advapi32.NewProc("InitializeAcl")
)

// Win32 failure rendered like pywintypes.error, code, function and message
type winAPIError struct {
	fn    string
	errno windows.Errno
}

func (e *winAPIError) Error() string {
	return fmt.Sprintf("(%d, %s, %s)", uint32(e.errno), pyrepr.StrRepr(e.fn), pyrepr.StrRepr(e.errno.Error()))
}

func apiErr(fn string, err error) error {
	var errno windows.Errno
	if errors.As(err, &errno) {
		return &winAPIError{fn, errno}
	}
	return err
}

// TokenElevation of pid's token, this process for nil, unknown when the process or its token cannot be opened
func ProcessElevation(pid *uint32) (elevated, known bool) {
	process := windows.CurrentProcess()
	if pid != nil {
		h, err := windows.OpenProcess(processQueryLimitedInformation, false, *pid)
		if err != nil || h == 0 {
			return false, false
		}
		defer windows.CloseHandle(h)
		process = h
	}
	var token windows.Token
	if err := windows.OpenProcessToken(process, tokenQuery, &token); err != nil {
		return false, false
	}
	defer token.Close()
	var value, size uint32
	if err := windows.GetTokenInformation(token, tokenElevation, (*byte)(unsafe.Pointer(&value)), uint32(unsafe.Sizeof(value)), &size); err != nil {
		return false, false
	}
	return value != 0, true
}

func probeProcessElevated() bool {
	elevated, known := ProcessElevation(nil)
	return known && elevated
}

func stripWin32Prefix(path string) string {
	if strings.HasPrefix(path, `\\?\UNC\`) {
		return `\\` + path[8:]
	}
	return strings.TrimPrefix(path, `\\?\`)
}

func finalPathName(h windows.Handle) (string, error) {
	n := uint32(windows.MAX_PATH)
	for {
		buf := make([]uint16, n)
		r, err := windows.GetFinalPathNameByHandle(h, &buf[0], n, 0)
		if err != nil {
			return "", apiErr("GetFinalPathNameByHandle", err)
		}
		if r < n {
			return windows.UTF16ToString(buf[:r]), nil
		}
		n = r
	}
}

func kernelObjectSecurity(h windows.Handle) (*windows.SECURITY_DESCRIPTOR, error) {
	var needed uint32
	procGetKernelObjectSecurity.Call(uintptr(h), uintptr(windows.DACL_SECURITY_INFORMATION), 0, 0, uintptr(unsafe.Pointer(&needed)))
	if needed == 0 {
		needed = 1
	}
	buf := make([]byte, needed)
	r, _, e := procGetKernelObjectSecurity.Call(uintptr(h), uintptr(windows.DACL_SECURITY_INFORMATION), uintptr(unsafe.Pointer(&buf[0])), uintptr(needed), uintptr(unsafe.Pointer(&needed)))
	if r == 0 {
		return nil, apiErr("GetKernelObjectSecurity", e)
	}
	return (*windows.SECURITY_DESCRIPTOR)(unsafe.Pointer(&buf[0])), nil
}

// Layout of the ACL header whose size field x/sys/windows keeps unexported
type aclHeader struct {
	_    [2]byte
	size uint16
	_    [4]byte
}

// AddAccessAllowedAceEx on a copy of acl, padded and retried when the ACL has no room like PyACL
func addAccessAllowedACE(acl *windows.ACL, flags uint32, sid *windows.SID) (*windows.ACL, error) {
	size := int((*aclHeader)(unsafe.Pointer(acl)).size)
	buf := make([]byte, size)
	copy(buf, unsafe.Slice((*byte)(unsafe.Pointer(acl)), size))
	add := func(b []byte) (uintptr, error) {
		r, _, e := procAddAccessAllowedAceEx.Call(uintptr(unsafe.Pointer(&b[0])), aclRevisionDS, uintptr(flags), fileAllAccess, uintptr(unsafe.Pointer(sid)))
		return r, e
	}
	if r, e := add(buf); r != 0 {
		return (*windows.ACL)(unsafe.Pointer(&buf[0])), nil
	} else if !errors.Is(e, windows.ERROR_ALLOTTED_SPACE_EXCEEDED) {
		return nil, apiErr("AddAccessAllowedAceEx", e)
	}
	required := uint16(size) + accessAllowedACESize + uint16(windows.GetLengthSid(sid))
	padded := make([]byte, required)
	copy(padded, buf)
	(*aclHeader)(unsafe.Pointer(&padded[0])).size = required
	if r, e := add(padded); r == 0 {
		return nil, apiErr("AddAccessAllowedAceEx", e)
	}
	return (*windows.ACL)(unsafe.Pointer(&padded[0])), nil
}

func emptyACL() (*windows.ACL, error) {
	buf := make([]byte, 64)
	if r, _, e := procInitializeAcl.Call(uintptr(unsafe.Pointer(&buf[0])), 64, aclRevision); r == 0 {
		return nil, apiErr("InitializeAcl", e)
	}
	return (*windows.ACL)(unsafe.Pointer(&buf[0])), nil
}

// Writes the user's full control ACE on the opened handle, true when written and false when already present
func grantNativeACL(path string, isDir bool, expected string) (bool, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return false, apiErr("OpenProcessToken", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return false, apiErr("GetTokenInformation", err)
	}
	p16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	h, err := windows.CreateFile(p16, readControl|writeDAC, fileShareAll, nil, openExisting, fileFlagBackupSemantics|fileFlagOpenReparsePoint, 0)
	if err != nil {
		return false, apiErr("CreateFile", err)
	}
	defer windows.CloseHandle(h)
	if expected != "" {
		final, err := finalPathName(h)
		if err != nil {
			return false, err
		}
		final = stripWin32Prefix(final)
		if pypath.Normcase(strings.TrimRight(final, `\`)) != pypath.Normcase(strings.TrimRight(expected, `\`)) {
			return false, &ReparsePointRefused{fmt.Sprintf("%s resolves to %s, not %s", path, final, expected)}
		}
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return false, apiErr("GetFileInformationByHandle", err)
	}
	if info.FileAttributes&FileAttributeReparsePoint != 0 {
		return false, &ReparsePointRefused{fmt.Sprintf("%s is a junction, symlink or other reparse point", path)}
	}
	if info.FileAttributes&FileAttributeDirectory == 0 && info.NumberOfLinks > 1 {
		return false, &ReparsePointRefused{fmt.Sprintf("%s is a hard link (%d names); its other names may be outside", path, info.NumberOfLinks)}
	}
	isDir = info.FileAttributes&FileAttributeDirectory != 0
	descriptor, err := kernelObjectSecurity(h)
	if err != nil {
		return false, err
	}
	absolute, err := descriptor.ToAbsolute()
	if err != nil {
		return false, apiErr("MakeAbsoluteSD", err)
	}
	dacl, _, err := absolute.DACL()
	if err != nil && !errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
		return false, apiErr("GetSecurityDescriptorDacl", err)
	}
	inherit := uint32(0)
	if isDir {
		inherit = objectInheritACE | containerInheritACE
	}
	if dacl != nil {
		for index := uint32(0); index < uint32(dacl.AceCount); index++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, index, &ace); err != nil {
				return false, apiErr("GetAce", err)
			}
			flags := uint32(ace.Header.AceFlags)
			if ace.Header.AceType == accessAllowedACEType && (*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(user.User.Sid) &&
				uint32(ace.Mask)&fileAllAccess == fileAllAccess && flags&inherit == inherit && flags&inheritOnlyACE == 0 {
				return false, nil
			}
		}
	} else if dacl, err = emptyACL(); err != nil {
		return false, err
	}
	dacl, err = addAccessAllowedACE(dacl, inherit, user.User.Sid)
	if err != nil {
		return false, err
	}
	if err := absolute.SetDACL(dacl, true, false); err != nil {
		return false, apiErr("SetSecurityDescriptorDacl", err)
	}
	if err := windows.SetKernelObjectSecurity(h, windows.DACL_SECURITY_INFORMATION, absolute); err != nil {
		return false, apiErr("SetKernelObjectSecurity", err)
	}
	return true, nil
}

type fileAttributeTagInfo struct {
	FileAttributes uint32
	ReparseTag     uint32
}

// os.lstat facts as CPython reads them on Windows, from the handle or the directory entry when access is refused
func lstatForACL(path string) (aclStat, error) {
	p16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return aclStat{}, err
	}
	h, err := windows.CreateFile(p16, windows.FILE_READ_ATTRIBUTES, 0, nil, openExisting, fileFlagBackupSemantics|fileFlagOpenReparsePoint, 0)
	if err != nil {
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return aclStat{}, &fs.PathError{Op: "lstat", Path: path, Err: err}
		}
		var fd windows.Win32finddata
		fh, ferr := windows.FindFirstFile(p16, &fd)
		if ferr != nil {
			return aclStat{}, &fs.PathError{Op: "lstat", Path: path, Err: ferr}
		}
		windows.FindClose(fh)
		st := aclStat{attrs: fd.FileAttributes}
		if fd.FileAttributes&FileAttributeReparsePoint != 0 {
			st.tag = fd.Reserved0
		}
		st.link = st.tag == ioReparseTagSymlink
		st.dir = st.attrs&FileAttributeDirectory != 0 && !st.link
		return st, nil
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return aclStat{}, &fs.PathError{Op: "lstat", Path: path, Err: err}
	}
	st := aclStat{attrs: info.FileAttributes, nlink: uint64(info.NumberOfLinks), dev: uint64(info.VolumeSerialNumber),
		ino: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)}
	if info.FileAttributes&FileAttributeReparsePoint != 0 {
		var tag fileAttributeTagInfo
		if err := windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo, (*byte)(unsafe.Pointer(&tag)), uint32(unsafe.Sizeof(tag))); err != nil {
			return aclStat{}, &fs.PathError{Op: "lstat", Path: path, Err: err}
		}
		st.tag = tag.ReparseTag
	}
	st.link = st.tag == ioReparseTagSymlink
	st.dir = st.attrs&FileAttributeDirectory != 0 && !st.link
	return st, nil
}

// os.mkdir, a mode of 0o700 applying the protected owner, SYSTEM and Administrators DACL CPython 3.13 applies
func osMkdir(p string, mode os.FileMode) error {
	if mode != 0o700 {
		return os.Mkdir(p, mode)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;OW)")
	if err != nil {
		return &fs.PathError{Op: "mkdir", Path: p, Err: err}
	}
	p16, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return &fs.PathError{Op: "mkdir", Path: p, Err: err}
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	if err := windows.CreateDirectory(p16, &sa); err != nil {
		return &fs.PathError{Op: "mkdir", Path: p, Err: err}
	}
	return nil
}
