//go:build windows

package fileutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CopyFileEx flag that lets an encrypted source land on a volume without encryption
const copyFileAllowDecryptedDestination = 0x8

// Copies src to dst with CopyFileEx, which carries attributes, streams and times, else bytes then metadata
func copyFile(src, dst string) error {
	from, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return &os.PathError{Op: "copy", Path: src, Err: err}
	}
	to, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return &os.PathError{Op: "copy", Path: dst, Err: err}
	}
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("CopyFileExW")
	if err := proc.Find(); err != nil {
		return &os.PathError{Op: "copy", Path: src, Err: err}
	}
	r, _, callErr := proc.Call(uintptr(unsafe.Pointer(from)), uintptr(unsafe.Pointer(to)), 0, 0, 0, copyFileAllowDecryptedDestination)
	if r != 0 {
		return nil
	}
	if !errors.Is(callErr, windows.ERROR_ACCESS_DENIED) {
		return &os.PathError{Op: "copy", Path: src, Err: callErr}
	}
	if err := copyData(src, dst); err != nil {
		return err
	}
	return copyMeta(src, dst, true)
}

// Copies access and modification times and the read-only attribute of src onto dst, a symlink taking neither
func copyMeta(src, dst string, follow bool) error {
	if !follow {
		return nil
	}
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	atime := fi.ModTime()
	if data, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		atime = time.Unix(0, data.LastAccessTime.Nanoseconds())
	}
	if err := os.Chtimes(dst, atime, fi.ModTime()); err != nil {
		return err
	}
	return os.Chmod(dst, fi.Mode().Perm())
}

// Lifts the read-only attribute that blocks deleting p
func unblock(p string) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return
	}
	attrs, err := windows.GetFileAttributes(name)
	if err != nil || attrs&windows.FILE_ATTRIBUTE_READONLY == 0 {
		return
	}
	_ = windows.SetFileAttributes(name, attrs&^windows.FILE_ATTRIBUTE_READONLY)
}

// Reports whether a directory entry is a real directory rather than a junction or directory symlink
func realDir(e fs.DirEntry) bool {
	if !e.IsDir() {
		return false
	}
	fi, err := e.Info()
	return err == nil && !reparsePoint(fi)
}

// Sharing and lock violations another handle causes, which Windows reports for files held open
func sharingViolation(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

// Reports whether name matches the glob pattern, ignoring case as Windows compares names
func matchName(pattern, name string) (bool, error) {
	return filepath.Match(strings.ToLower(pattern), strings.ToLower(name))
}

// Stats the source as a directory move does, Windows keeping no immutable flag to refuse on
func moveRefused(src string) (bool, error) {
	_, err := os.Stat(src)
	return false, err
}

// Windows tree copies leave a directory whose times or attributes could not be copied out of the failures
const dirMetaReported = false

// Renames src to dst with MoveFileEx, refusing a file already at dst
func renameNoReplace(src, dst string) error { return moveFile(src, dst, 0) }
