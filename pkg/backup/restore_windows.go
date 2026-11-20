//go:build windows

package backup

import (
	"os"
	"syscall"
)

// Hard link count of the file path names, following a symlink
func linkCount(path string) (uint64, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	h, err := syscall.CreateFile(p, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	defer syscall.CloseHandle(h)
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		return 0, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	return uint64(info.NumberOfLinks), nil
}

// Mode bits Python synthesizes for a stat result
func statMode(st os.FileInfo) uint32 { return uint32(st.Mode().Perm()) }

// chmod as Windows applies it, only the write bit taking effect
func chmodFull(path string, mode uint32) error { return os.Chmod(path, os.FileMode(mode&0o777)) }

// Windows has no fchmod, the staged file takes the mode through its path
func fchmodStaged(_ *os.File, name string, mode uint32) error { return chmodFull(name, mode) }

// Mode open(path, "wb") gives a new file under the C runtime's default umask of zero
func processNewFileMode() (uint32, bool) { return 0o666, true }
