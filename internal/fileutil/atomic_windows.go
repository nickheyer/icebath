//go:build windows

package fileutil

import (
	"context"
	"errors"
	"io/fs"
	"math"
	"math/rand/v2"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// A rename asked to sync uses MOVEFILE_WRITE_THROUGH, which makes the directory entry durable itself
const renameWritesThrough = true

// Renames retried while another handle holds the target before the write goes through that handle
const replaceRetryAttempts = 4

// Windows files carry no POSIX owner to hand back
type fileOwner struct{}

// Temporary file in dir, created under a protected owner-only DACL when the write is private
func createTemp(dir string, cfg writeConfig) (*os.File, error) {
	if cfg.private {
		return createPrivateTemp(dir, cfg.prefix, cfg.suffix)
	}
	return os.CreateTemp(dir, cfg.prefix+"*"+cfg.suffix)
}

// Windows applies the mode after publishing, so the open temp file is left alone
func applyStagedMode(*os.File, metaPlan) error { return nil }

// A replaced target keeps the attributes of the temporary file without KeepPerm or Perm
func stagedTempMode() (fs.FileMode, bool) { return 0, false }

// Windows synthesizes mode bits, so a new file gets no explicit mode
func defaultNewFileMode() (fs.FileMode, bool) { return 0, false }

// Windows files have no POSIX owner
func ownerOf(fs.FileInfo) *fileOwner { return nil }

// Windows replaces never change a POSIX owner
func restoreOwner(string, *fileOwner) {}

// Windows lets any writer with attribute access toggle the read-only attribute a mode maps to
func mayChmod(string) bool { return true }

// Puts a kept target inode under the private DACL before new bytes land in it when the write is private
func secureKeptInode(_ *os.File, path string, _ fs.FileMode, private bool) error {
	if !private {
		return nil
	}
	return protectPrivateFile(path)
}

// Renames tmp over the resolved target, riding out a held target and copying across volumes
func replace(ctx context.Context, tmp, target string, syncDir, private bool) (string, bool, error) {
	published, err := publishPath(target)
	if err != nil {
		return "", false, err
	}
	flags := uint32(windows.MOVEFILE_REPLACE_EXISTING)
	if syncDir {
		flags |= windows.MOVEFILE_WRITE_THROUGH
	}
	err = moveFile(tmp, published, flags)
	if err == nil {
		return published, false, nil
	}
	contended := replaceContended(err)
	if !replaceCrossDevice(err) && !contended {
		return "", false, err
	}
	if contended {
		for attempt := 1; attempt <= replaceRetryAttempts; attempt++ {
			if err := sleepContext(ctx, replaceBackoff(attempt)); err != nil {
				return "", false, err
			}
			retryErr := moveFile(tmp, published, flags)
			if retryErr == nil {
				return published, false, nil
			}
			if replaceCrossDevice(retryErr) {
				contended = false
				break
			}
			if !replaceContended(retryErr) {
				return "", false, retryErr
			}
		}
	}
	if contended {
		err = rewriteInPlace(tmp, published, private)
	} else {
		err = copyOver(tmp, published, private)
	}
	if err != nil {
		return "", false, err
	}
	return published, true, nil
}

// MoveFileEx with the given flags, failures reported as a rename link error
func moveFile(from, to string, flags uint32) error {
	src, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	dst, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	if err := windows.MoveFileEx(src, dst, flags); err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
}

// Access denied, sharing and lock violations a rename reports while another handle holds the target
func replaceContended(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

// Different volume and busy target refusals that no retry can clear
func replaceCrossDevice(err error) bool {
	return errors.Is(err, windows.ERROR_NOT_SAME_DEVICE) || errors.Is(err, windows.ERROR_BUSY)
}

// Delay before retry attempt n, doubling from 20ms to a 100ms cap plus up to half again of jitter
func replaceBackoff(attempt int) time.Duration {
	delay := math.Min(0.02*math.Pow(2, float64(attempt-1)), 0.1)
	return time.Duration((delay + rand.Float64()*0.5*delay) * float64(time.Second))
}

// Overwrites the still held target through a handle of its own, privately secured first when asked, then unlinks tmp
func rewriteInPlace(tmp, target string, private bool) error {
	data, err := os.ReadFile(tmp)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if err := secureKeptInode(f, target, 0, private); err != nil {
		return joinErr(err, f.Close())
	}
	if _, err := f.WriteAt(data, 0); err != nil {
		return joinErr(err, f.Close())
	}
	if err := f.Truncate(int64(len(data))); err != nil {
		return joinErr(err, f.Close())
	}
	flushKept(f)
	if err := f.Close(); err != nil {
		return err
	}
	return os.Remove(tmp)
}

// Flushes a directory handle so entries created, renamed or removed inside it reach the disk, every failure reported
func FsyncDir(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	flushErr := windows.FlushFileBuffers(h)
	closeErr := windows.CloseHandle(h)
	if flushErr != nil {
		return &os.PathError{Op: "sync", Path: path, Err: flushErr}
	}
	if closeErr != nil {
		return &os.PathError{Op: "close", Path: path, Err: closeErr}
	}
	return nil
}
