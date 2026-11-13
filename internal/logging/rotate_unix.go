//go:build unix

package logging

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"time"
)

// Line ending the log files are written with
const lineEnding = "\n"

// Characters an export file name never takes from a session id, the path separator here
const nameSeparators = "/"

// What a POSIX write does once the lock file cannot be locked
const lockFallback = "rotation proceeds without it"

// Open append stream of a POSIX log and the identity of the file it was opened on
type platformFile struct {
	out  *os.File
	snap fs.FileInfo
}

// Delays a POSIX rename waits through, none since an open file never blocks a rename
func renameDelays() []time.Duration { return nil }

// Opens the log at once so a managed home sees it created with its group mode, then records its identity
func (f *rotatingFile) prepare() error {
	if err := f.open(); err != nil {
		return err
	}
	f.snap, _ = os.Stat(f.path)
	return nil
}

// Opens the log for appending, creating it with the umask applied mode
func (f *rotatingFile) open() error {
	out, err := os.OpenFile(f.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return err
	}
	f.out = out
	f.chmodManaged()
	return nil
}

// Closes the append stream, the next write reopening it
func (f *rotatingFile) dropStream() {
	if f.out != nil {
		_ = f.out.Close()
		f.out = nil
	}
}

// Keeps rotating without the lock on a filesystem that cannot lock, as every POSIX writer did before the lock existed
func (f *rotatingFile) lockUnavailable() bool { return true }

// Appends one line, following an outside rotation, skipping a vanished profile and rotating by size under the lock
func (f *rotatingFile) write(line string) {
	if f.closed {
		return
	}
	f.skipped = false
	had := f.out != nil
	exists := had || pathExists(f.path)
	if exists {
		f.reopenIfRotated()
	}
	if f.out == nil && (had || !exists) && f.skipMissingDir && !isDir(f.dir) {
		f.skipped = true
		return
	}
	rotate := true
	if err := f.lock(); err == nil {
		defer f.unlock()
		f.reopenIfRotated()
	} else {
		rotate = f.lockFailed(err)
	}
	if f.out == nil && f.skipMissingDir && !isDir(f.dir) {
		f.skipped = true
		return
	}
	if err := f.emit(line, rotate); err != nil {
		f.fail(line, err)
	}
	if f.out != nil {
		f.reported = false
	}
}

// Rotates first when rotation is allowed and the line would reach the size limit, then appends it
func (f *rotatingFile) emit(line string, rotate bool) error {
	text := line + lineEnding
	if rotate {
		roll, err := f.shouldRollover(len(text))
		if err != nil {
			return err
		}
		if roll {
			if err := f.rollover(); err != nil {
				f.rolloverFailed(err)
			}
		}
	}
	if f.out == nil {
		if err := f.open(); err != nil {
			return err
		}
	}
	_, err := f.out.WriteString(text)
	return err
}

// Reports whether appending size bytes to a non-empty regular log reaches the size limit
func (f *rotatingFile) shouldRollover(size int) (bool, error) {
	if f.out == nil {
		if err := f.open(); err != nil {
			return false, err
		}
	}
	if f.maxBytes <= 0 {
		return false, nil
	}
	st, err := f.out.Stat()
	if err != nil {
		return false, err
	}
	if st.Size() == 0 || float64(st.Size())+float64(size) < f.maxBytes {
		return false, nil
	}
	if cur, err := os.Stat(f.path); err == nil && !cur.Mode().IsRegular() {
		return false, nil
	}
	return true, nil
}

// Shifts the backups up, moves the log to the first, reopens it and hands it the old owner and mode
func (f *rotatingFile) rollover() error {
	previous, perr := os.Stat(f.path)
	f.dropStream()
	var shiftErr error
	if f.backups > 0 {
		for i := f.backups - 1; i > 0 && shiftErr == nil; i-- {
			if src := backupName(f.path, i); pathExists(src) {
				shiftErr = moveOver(src, backupName(f.path, i+1))
			}
		}
		if shiftErr == nil {
			shiftErr = moveOver(f.path, backupName(f.path, 1))
		}
	}
	if err := f.open(); err != nil {
		return errors.Join(shiftErr, err)
	}
	if shiftErr != nil {
		f.snap, _ = os.Stat(f.path)
		return shiftErr
	}
	if perr == nil {
		handOff(f.path, previous)
	}
	f.chmodManaged()
	f.snap, _ = os.Stat(f.path)
	return nil
}

// Gives a fresh log the mode the rotated out file had and, when running as root, its owner too
func handOff(path string, previous fs.FileInfo) {
	if os.Geteuid() == 0 {
		if st, ok := previous.Sys().(*syscall.Stat_t); ok && os.Chown(path, int(st.Uid), int(st.Gid)) != nil {
			return
		}
	}
	_ = os.Chmod(path, previous.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky))
}

// Reopens the log when its path was renamed, unlinked or replaced since the stream was opened
func (f *rotatingFile) reopenIfRotated() {
	st, err := os.Stat(f.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		f.reopen(nil)
	case err != nil:
	case f.snap == nil:
		f.snap = st
	case !os.SameFile(st, f.snap):
		f.reopen(st)
	}
}

// Closes and opens the log again at its path, leaving no stream when that fails
func (f *rotatingFile) reopen(st fs.FileInfo) {
	f.dropStream()
	if f.open() != nil {
		return
	}
	if st == nil {
		st, _ = os.Stat(f.path)
	}
	f.snap = st
}
