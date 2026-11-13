package home

import (
	"errors"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Line ending text files and the prune audit log are written with
const lineEnding = "\r\n"

// A permission failure on Windows is usually a transient open handle, so every pass retries instead of remembering
const stuckTrackingSupported = false

// Directory entries on Windows carry no link count, so no file is exempted as a hard link
func linkCount(fs.FileInfo) uint64 { return 0 }

// Reports whether a stat failure means the path is not a directory to follow, device and name errors included
func ignorableStatError(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, windows.ERROR_NOT_READY) ||
		errors.Is(err, windows.ERROR_INVALID_NAME) || errors.Is(err, windows.ERROR_CANT_RESOLVE_FILENAME) ||
		errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EBADF)
}

// Windows lists no POSIX owner, so every process whose working directory is readable is a candidate
func sameUser(int) bool { return true }

// File index, modification and creation time of entry itself, false when it cannot be examined
func entryFingerprint(entry string) (fingerprint, bool) {
	id, err := fileutil.LstatID(entry)
	if err != nil {
		return fingerprint{}, false
	}
	fi, err := os.Lstat(entry)
	if err != nil {
		return fingerprint{}, false
	}
	d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return fingerprint{}, false
	}
	return fingerprint{ino: id.Inode, mtime: d.LastWriteTime.Nanoseconds(), ctime: d.CreationTime.Nanoseconds()}, true
}

// Reports whether path is gone, or exists and is a directory or a file without the read-only attribute
func accessibleNow(path string) bool {
	if _, err := os.Lstat(path); err != nil {
		return true
	}
	return fileutil.Accessible(path, fileutil.AccessRead|fileutil.AccessWrite|fileutil.AccessExec)
}
