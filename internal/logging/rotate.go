package logging

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Times a write reopens a lock file that another process replaced before it gives up on the lock
const lockReopenLimit = 8

// Longest a write waits on a lock another process holds before it appends without rotating
const lockWait = 9 * time.Second

// Log file every icebath process appends to, rotated by size under a lock file beside it
type rotatingFile struct {
	platformFile
	s              *System
	path           string
	dir            string
	lockPath       string
	maxBytes       float64
	backups        int
	managed        bool
	skipMissingDir bool
	skipped        bool
	closed         bool
	reported       bool
	contended      bool
	lockFile       *os.File
}

// Log file at path rotated past maxBytes into backups numbered copies, its lock file named after its stem
func (s *System) openRotating(path string, maxBytes float64, backups int, managed, skipMissingDir bool) (*rotatingFile, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir, base := filepath.Split(abs)
	f := &rotatingFile{
		s:              s,
		path:           abs,
		dir:            filepath.Clean(dir),
		lockPath:       filepath.Join(dir, ".__"+strings.TrimSuffix(base, ".log")+".lock"),
		maxBytes:       maxBytes,
		backups:        backups,
		managed:        managed,
		skipMissingDir: skipMissingDir,
	}
	if err := f.prepare(); err != nil {
		return nil, err
	}
	return f, nil
}

// Appends one line as UTF-8 text, every invalid sequence written as the replacement character
func (f *rotatingFile) append(line string) { f.write(decodeReplacing([]byte(line))) }

// Takes the cross-process claim beside the log for one write, reopening its file when another process replaced it
func (f *rotatingFile) lock() error {
	for range lockReopenLimit {
		if f.lockFile == nil {
			lf, err := openLockFile(f.lockPath)
			if err != nil {
				return err
			}
			if f.managed {
				_ = os.Chmod(f.lockPath, 0o660)
			}
			f.lockFile = lf
		}
		if err := f.acquire(); err != nil {
			return err
		}
		held, herr := f.lockFile.Stat()
		now, nerr := os.Stat(f.lockPath)
		if herr == nil && nerr == nil && os.SameFile(held, now) {
			if f.contended {
				f.contended = false
				f.s.logSelf(slog.LevelInfo, "cross-process log lock for "+f.path+" is free again, rotation resumed")
			}
			return nil
		}
		_ = fileutil.UnlockFile(f.lockFile)
		_ = f.lockFile.Close()
		f.lockFile = nil
		if nerr != nil && !errors.Is(nerr, fs.ErrNotExist) {
			return nerr
		}
	}
	return fmt.Errorf("lock file %s keeps being replaced", f.lockPath)
}

// Waits up to lockWait for the lock, only trying it once while another process is known to sit on it
func (f *rotatingFile) acquire() error {
	if f.contended {
		return fileutil.TryLockFile(f.lockFile)
	}
	ctx, cancel := context.WithTimeout(context.Background(), lockWait)
	defer cancel()
	return fileutil.LockFile(ctx, f.lockFile)
}

// Releases the lock taken for the current write
func (f *rotatingFile) unlock() { _ = fileutil.UnlockFile(f.lockFile) }

// Lock file opened for locking, read only when its mode refuses this user writing
func openLockFile(path string) (*os.File, error) {
	lf, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o666)
	if errors.Is(err, fs.ErrPermission) {
		return os.Open(path)
	}
	return lf, err
}

// Handles a failed lock for a write that goes ahead, true when that write may still rotate the log
func (f *rotatingFile) lockFailed(err error) bool {
	switch {
	case errors.Is(err, fileutil.ErrLocked):
		if !f.contended {
			f.contended = true
			f.s.logSelf(slog.LevelWarn, fmt.Sprintf("timed out after %s waiting for the cross-process log lock of %s, "+
				"records are appended without rotation until the process holding it lets go", lockWait, f.path))
		}
		return false
	case errors.Is(err, fs.ErrNotExist):
		return true
	}
	rotate := f.lockUnavailable()
	if f.s.firstWarning("lock-unavailable:" + f.path) {
		f.s.logSelf(slog.LevelWarn, fmt.Sprintf("cross-process log locking unavailable for %s (%s); %s", f.path, errText(err), lockFallback))
	}
	return rotate
}

// Reports a failed write, naming an unavailable destination once and dropping its stream until it recovers
func (f *rotatingFile) fail(line string, err error) {
	if errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed) {
		if !f.reported {
			f.reported = true
			_, _ = fmt.Fprintf(SafeStderr(), "icebath logging: %s unavailable (%s); file logging paused until it recovers\n", f.path, errText(err))
		}
		f.dropStream()
		return
	}
	reportError(line, err)
}

// Warns once per log that a rollover could not finish, the log growing past its limit until one does
func (f *rotatingFile) rolloverFailed(err error) {
	if f.s.firstWarning("rollover:" + f.path) {
		f.s.logSelf(slog.LevelWarn, fmt.Sprintf("cannot rotate %s (%v); records keep being appended past its size limit until a rotation succeeds",
			f.path, err))
	}
}

// Gives the log the group read and write mode a managed install shares its logs with
func (f *rotatingFile) chmodManaged() {
	if f.managed {
		_ = os.Chmod(f.path, 0o660)
	}
}

// Closes the log and its lock file for good, later writes dropped
func (f *rotatingFile) close() {
	f.closed = true
	f.dropStream()
	if f.lockFile != nil {
		_ = f.lockFile.Close()
		f.lockFile = nil
	}
}

// Name of the numbered backup n of the log at path
func backupName(path string, n int) string { return path + "." + strconv.Itoa(n) }

// Moves src over dst when src exists, removing dst first and riding out a held file on Windows
func moveOver(src, dst string) error {
	if pathExists(dst) {
		if err := os.Remove(dst); err != nil {
			return err
		}
	}
	if !pathExists(src) {
		return nil
	}
	return fileutil.RenameRetrying(context.Background(), src, dst, nil, renameDelays()...)
}

// Reports whether something is at path, following links
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Reports whether path is a directory, following links
func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// Text of the system error under err, without the operation and path wrapping it
func errText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err.Error()
	}
	return err.Error()
}

// Prints the logging error report for a record that could not be written, its text already redacted
func reportError(line string, err error) {
	_, _ = fmt.Fprintf(SafeStderr(), "--- Logging error ---\n%v\nMessage: %s\n", err, strconv.Quote(line))
}

// Logs a message about the logging itself at level through every sink
func (s *System) logSelf(level slog.Level, msg string) {
	s.emit(context.Background(), &record{time: time.Now(), level: level, name: "logging", message: msg})
}
