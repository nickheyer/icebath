package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var (
	goos      = runtime.GOOS
	lockFile  = pyfs.LockFile
	chown     = os.Chown
	geteuid   = os.Geteuid
	isManaged = func(ctx context.Context) bool { return config.IsManaged(ctx, home.Dir(ctx)) }
)

var (
	clhMu               sync.Mutex
	useCLH              bool
	windowsCLHFallback  bool
	windowsCLHReason    string
	fallbackWarned      bool
	lockTimeoutWarned   bool
	lockTimeoutWarnLock sync.Mutex
)

func init() { selectRotation() }

// Picks the cross process locking rotation on Windows, the plain rotation without rollover when locking fails there
func selectRotation() {
	clhMu.Lock()
	defer clhMu.Unlock()
	useCLH, windowsCLHFallback, windowsCLHReason = false, false, ""
	if goos != "windows" {
		return
	}
	if ok, reason := portalockerProbe(); ok {
		useCLH = true
	} else {
		windowsCLHFallback, windowsCLHReason = true, reason
	}
}

// Takes and releases an exclusive lock on a scratch file, the failure repr when this box cannot lock
func portalockerProbe() (bool, string) {
	if goos != "windows" {
		return true, ""
	}
	f, err := os.CreateTemp("", "icebath-portalocker-")
	if err != nil {
		return false, errorRepr(err)
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	if err := lockFile(f); err != nil {
		return false, errorRepr(err)
	}
	if err := pyfs.UnlockFile(f); err != nil {
		return false, errorRepr(err)
	}
	return true, ""
}

// Python repr of the exception an error stands for
func errorRepr(err error) string {
	return errorClass(err) + "(" + pyrepr.StrRepr(errorText(err)) + ")"
}

// Lock timeout concurrent-log-handler raises after its retries
type lockTimeoutError struct{}

func (*lockTimeoutError) Error() string { return "Cannot acquire lock after 20 attempts" }

// Reports whether err is the concurrent-log-handler lock timeout on a Windows host
func isWindowsConcurrentLogLockTimeout(err error) bool {
	return goos == "windows" && err != nil && errorClass(err) == "RuntimeError" && strings.Contains(err.Error(), "Cannot acquire lock after 20 attempts")
}

// Warns once per process that a cross process log lock timed out and records were dropped
func warnWindowsLockTimeoutOnce() {
	lockTimeoutWarnLock.Lock()
	if lockTimeoutWarned {
		lockTimeoutWarnLock.Unlock()
		return
	}
	lockTimeoutWarned = true
	lockTimeoutWarnLock.Unlock()
	GetLogger("hermes_logging").Warning("concurrent-log-handler timed out acquiring the cross-process log " +
		"lock; this and later records were dropped (the Desktop slash-worker " +
		"surface stays clean, but file logging is degraded).")
}

// Reports whether a file handler lost its backing stream to EIO or a closed file
func isUnavailableLogStream(err error) bool {
	return errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed)
}

// Open log file the handler appends to
type logStream interface {
	io.Writer
	Tell() (int64, error)
	Close() error
}

// Append mode file positioned at its end the way Python opens mode a
type appendFile struct{ *os.File }

func (f appendFile) Tell() (int64, error) { return f.Seek(0, io.SeekCurrent) }

// Opens path for appending, creating it with the umask applied mode 0666
func openAppend(path string) (logStream, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		_ = f.Close()
		return nil, err
	}
	return appendFile{f}, nil
}

// Rotating file handler with managed mode perms, external rotation recovery and Windows cross process locking
type managedRotatingFileHandler struct {
	handlerCore
	baseFilename            string
	maxBytes                float64
	backupCount             int
	clh                     bool
	managed                 bool
	open                    func(path string) (logStream, error)
	stream                  logStream
	closed                  bool
	unavailableReported     bool
	skipWriteWhenDirMissing bool
	skippedMissingDir       bool
	neverReopenAfterClose   bool
	stat                    os.FileInfo
}

// Handler for path, opened at once off Windows and lazily under the Windows lock
func newManagedRotatingFileHandler(ctx context.Context, path string, maxBytes float64, backupCount int) (*managedRotatingFileHandler, error) {
	abs, err := pypath.Abspath(path)
	if err != nil {
		return nil, err
	}
	clhMu.Lock()
	clh := useCLH
	clhMu.Unlock()
	h := &managedRotatingFileHandler{baseFilename: abs, maxBytes: maxBytes, backupCount: backupCount, clh: clh, open: openAppend}
	h.managed = isManaged(ctx)
	if !clh {
		s, err := h.openStream()
		if err != nil {
			return nil, err
		}
		h.stream = s
	}
	h.recordStreamStat(nil)
	registerHandler(h)
	return h, nil
}

// Gives the log file group read and write in managed mode
func (h *managedRotatingFileHandler) chmodIfManaged() {
	if h.managed {
		_ = os.Chmod(h.baseFilename, 0o660)
	}
}

// Opens the log file and applies the managed mode perms
func (h *managedRotatingFileHandler) openStream() (logStream, error) {
	s, err := h.open(h.baseFilename)
	if err != nil {
		return nil, err
	}
	h.chmodIfManaged()
	return s, nil
}

// Snapshots the identity of the log file so an emit can spot external rotation
func (h *managedRotatingFileHandler) recordStreamStat(st os.FileInfo) {
	if st == nil {
		var err error
		if st, err = os.Stat(h.baseFilename); err != nil {
			h.stat = nil
			return
		}
	}
	h.stat = st
}

// Closes and reopens the log file, leaving no stream when the reopen fails
func (h *managedRotatingFileHandler) reopenStream(st os.FileInfo) {
	if h.stream != nil {
		quietly(h.stream.Close)
	}
	h.stream = nil
	s, err := h.openStream()
	if err != nil {
		return
	}
	if h.clh {
		quietly(s.Close)
	} else {
		h.stream = s
	}
	h.recordStreamStat(st)
}

// Reopens when the path was renamed, unlinked or replaced by another file
func (h *managedRotatingFileHandler) reopenIfExternallyRotated() {
	st, err := os.Stat(h.baseFilename)
	if errors.Is(err, os.ErrNotExist) {
		h.reopenStream(nil)
		return
	}
	if err != nil {
		return
	}
	if h.stat == nil {
		h.recordStreamStat(st)
	} else if !os.SameFile(st, h.stat) {
		h.reopenStream(st)
	}
}

func (h *managedRotatingFileHandler) Handle(ctx context.Context, rec *Record) bool {
	if !h.filter(rec) {
		return false
	}
	h.lock.Lock()
	defer h.lock.Unlock()
	h.emit(ctx, rec)
	return true
}

// Writes rec unless the handler was retired or its profile logs directory vanished
func (h *managedRotatingFileHandler) emit(ctx context.Context, rec *Record) {
	h.skippedMissingDir = false
	if h.neverReopenAfterClose && h.closed {
		return
	}
	hadStream := h.stream != nil
	fileExists := hadStream || pathExists(h.baseFilename)
	if fileExists {
		h.reopenIfExternallyRotated()
	}
	if h.stream == nil && (hadStream || !fileExists) && h.skipWriteWhenDirMissing && !isDir(filepath.Dir(h.baseFilename)) {
		h.skippedMissingDir = true
		return
	}
	var err error
	if h.clh {
		err = h.clhEmit(ctx, rec)
	} else {
		err = h.rotatingEmit(ctx, rec)
	}
	if err != nil {
		h.handleError(rec, err)
	}
	if h.stream != nil {
		h.unavailableReported = false
	}
}

// Rolls over when the next line crosses maxBytes, then appends the line
func (h *managedRotatingFileHandler) rotatingEmit(ctx context.Context, rec *Record) error {
	msg, err := h.Formatter().Format(ctx, rec)
	if err != nil {
		return err
	}
	roll, err := h.shouldRollover(msg)
	if err != nil {
		return err
	}
	if roll {
		if err := h.doRollover(); err != nil {
			return err
		}
	}
	if h.stream == nil {
		s, err := h.openStream()
		if err != nil {
			return err
		}
		h.stream = s
	}
	_, err = io.WriteString(h.stream, msg+"\n")
	return err
}

// Reports whether appending msg reaches maxBytes on a regular, non empty file
func (h *managedRotatingFileHandler) shouldRollover(msg string) (bool, error) {
	if st, err := os.Stat(h.baseFilename); err == nil && !st.Mode().IsRegular() {
		return false, nil
	}
	if h.stream == nil {
		s, err := h.openStream()
		if err != nil {
			return false, err
		}
		h.stream = s
	}
	if h.maxBytes <= 0 {
		return false, nil
	}
	pos, err := h.stream.Tell()
	if err != nil {
		return false, err
	}
	if pos == 0 {
		return false, nil
	}
	if float64(pos)+float64(utf8.RuneCountInString(msg)+1) >= h.maxBytes {
		if st, err := os.Stat(h.baseFilename); err == nil && !st.Mode().IsRegular() {
			return false, nil
		}
		return true, nil
	}
	return false, nil
}

// Rotates the file, hands the new file back to the previous owner and mode, then refreshes the snapshot
func (h *managedRotatingFileHandler) doRollover() error {
	previous, prevErr := os.Stat(h.baseFilename)
	var err error
	if h.clh {
		err = h.clhRollover()
	} else {
		err = h.stdRollover()
	}
	if err != nil {
		return err
	}
	if prevErr == nil {
		func() {
			if geteuid() == 0 {
				if uid, gid, ok := fileOwner(previous); ok {
					if chown(h.baseFilename, uid, gid) != nil {
						return
					}
				}
			}
			_ = os.Chmod(h.baseFilename, previous.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky))
		}()
	}
	h.chmodIfManaged()
	h.recordStreamStat(nil)
	return nil
}

// Shifts numbered backups up one and renames the live file to .1, then reopens it
func (h *managedRotatingFileHandler) stdRollover() error {
	if h.stream != nil {
		_ = h.stream.Close()
		h.stream = nil
	}
	if h.backupCount > 0 {
		for i := h.backupCount - 1; i > 0; i-- {
			sfn := fmt.Sprintf("%s.%d", h.baseFilename, i)
			dfn := fmt.Sprintf("%s.%d", h.baseFilename, i+1)
			if pathExists(sfn) {
				if pathExists(dfn) {
					if err := os.Remove(dfn); err != nil {
						return err
					}
				}
				if err := os.Rename(sfn, dfn); err != nil {
					return err
				}
			}
		}
		dfn := h.baseFilename + ".1"
		if pathExists(dfn) {
			if err := os.Remove(dfn); err != nil {
				return err
			}
		}
		if pathExists(h.baseFilename) {
			if err := os.Rename(h.baseFilename, dfn); err != nil {
				return err
			}
		}
	}
	s, err := h.openStream()
	if err != nil {
		return err
	}
	h.stream = s
	return nil
}

// Lock file beside the log, .__agent.lock for agent.log
func (h *managedRotatingFileHandler) lockPath() string {
	base := strings.TrimSuffix(h.baseFilename, ".log") + ".lock"
	return filepath.Join(filepath.Dir(base), ".__"+filepath.Base(base))
}

// Takes the cross process lock, retrying twenty times before the timeout error
func (h *managedRotatingFileHandler) acquireLock() (*os.File, error) {
	f, err := os.OpenFile(h.lockPath(), os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		return nil, err
	}
	h.chmodLockIfManaged()
	for range 20 {
		if lockFile(f) == nil {
			return f, nil
		}
	}
	_ = f.Close()
	return nil, &lockTimeoutError{}
}

// Gives the lock file the managed mode perms the log file gets
func (h *managedRotatingFileHandler) chmodLockIfManaged() {
	if h.managed {
		_ = os.Chmod(h.lockPath(), 0o660)
	}
}

// Formats, locks, rolls over when the file reached maxBytes, writes the line and closes the file again
func (h *managedRotatingFileHandler) clhEmit(ctx context.Context, rec *Record) error {
	msg, err := h.Formatter().Format(ctx, rec)
	if err != nil {
		return err
	}
	lock, err := h.acquireLock()
	if err != nil {
		return err
	}
	defer func() {
		_ = pyfs.UnlockFile(lock)
		_ = lock.Close()
	}()
	if roll, err := h.clhShouldRollover(); err == nil && roll {
		_ = h.doRollover()
	}
	if h.stream != nil {
		_ = h.stream.Close()
		h.stream = nil
	}
	s, err := h.openStream()
	if err != nil {
		return err
	}
	_, werr := io.WriteString(s, msg+"\n")
	cerr := s.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// Reports whether the file already holds maxBytes or more
func (h *managedRotatingFileHandler) clhShouldRollover() (bool, error) {
	if h.maxBytes <= 0 {
		return false, nil
	}
	s, err := h.openStream()
	if err != nil {
		return false, err
	}
	defer func() { _ = s.Close() }()
	pos, err := s.Tell()
	if err != nil {
		return false, err
	}
	return float64(pos) >= h.maxBytes, nil
}

// Renames the live file through a temporary name to .1 after shifting backups, truncating when no backups are kept
func (h *managedRotatingFileHandler) clhRollover() error {
	if h.stream != nil {
		_ = h.stream.Close()
		h.stream = nil
	}
	if h.backupCount <= 0 {
		f, err := os.OpenFile(h.baseFilename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
		if err != nil {
			return err
		}
		h.chmodIfManaged()
		return f.Close()
	}
	tmp := ""
	for tmp == "" || pathExists(tmp) {
		tmp = fmt.Sprintf("%s.rotate.%08d", h.baseFilename, rand.IntN(100000000))
	}
	if os.Rename(h.baseFilename, tmp) != nil {
		return nil
	}
	rename := func(src, dst string) error {
		if pathExists(dst) {
			if err := os.Remove(dst); err != nil {
				return err
			}
		}
		if pathExists(src) {
			return os.Rename(src, dst)
		}
		return nil
	}
	for i := h.backupCount - 1; i > 0; i-- {
		sfn := fmt.Sprintf("%s.%d", h.baseFilename, i)
		if pathExists(sfn) {
			if err := rename(sfn, fmt.Sprintf("%s.%d", h.baseFilename, i+1)); err != nil {
				return err
			}
		}
	}
	return rename(tmp, h.baseFilename+".1")
}

// Silences the Windows lock timeout and names an unavailable destination once instead of a traceback per record
func (h *managedRotatingFileHandler) handleError(rec *Record, err error) {
	if isWindowsConcurrentLogLockTimeout(err) {
		warnWindowsLockTimeoutOnce()
		return
	}
	if isUnavailableLogStream(err) {
		if !h.unavailableReported {
			h.unavailableReported = true
			text := errorText(err)
			if t, ok := pypath.ErrnoText(err); ok {
				text = t
			}
			_, _ = fmt.Fprintf(safeStderr(), "hermes_logging: %s unavailable (%s); file logging paused until it recovers\n", h.baseFilename, text)
		}
		if h.stream != nil {
			quietly(h.stream.Close)
		}
		h.stream = nil
		return
	}
	handleErrorBase(rec, err)
}

// Closes the stream for good, a retired handler never reopening it
func (h *managedRotatingFileHandler) Close() error {
	h.lock.Lock()
	if h.stream != nil {
		quietly(h.stream.Close)
		h.stream = nil
	}
	h.closed = true
	h.lock.Unlock()
	unregisterHandler(h)
	return nil
}

// Stream the handler holds, nil once closed or between writes under the Windows lock
func (h *managedRotatingFileHandler) currentStream() logStream {
	h.lock.Lock()
	defer h.lock.Unlock()
	return h.stream
}

// Reports whether path exists
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Reports whether path is a directory
func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
