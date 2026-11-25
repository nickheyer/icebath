package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

// Lock file another holder kept past the timeout
type FileLockTimeoutError struct{ Message string }

func (e *FileLockTimeoutError) Error() string { return e.Message }

// Reentrant holder of a cross process file lock, keyed by the goroutine that took it
type fileLockHolder struct {
	mu    sync.Mutex
	owner uint64
	depth int
}

// Reentrant cross process file lock holder
type FileLockHolder = fileLockHolder

// Takes an advisory lock file within the timeout, reentrant for the holding goroutine, stamping the holder pid
func acquireFileLock(lockPath string, holder *fileLockHolder, timeout time.Duration, timeoutMessage string) (func(), error) {
	gid := lockGoroutineID()
	holder.mu.Lock()
	if holder.depth > 0 && holder.owner == gid {
		holder.depth++
		holder.mu.Unlock()
		return func() {
			holder.mu.Lock()
			holder.depth--
			holder.mu.Unlock()
		}, nil
	}
	holder.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o777); err != nil {
		return nil, err
	}
	flags := os.O_RDWR | os.O_CREATE | os.O_APPEND
	if isWindowsHost {
		if st, err := os.Stat(lockPath); err != nil || st.Size() == 0 {
			_ = os.WriteFile(lockPath, []byte(" "), 0o666)
		}
		flags = os.O_RDWR
	}
	f, err := os.OpenFile(lockPath, flags, 0o666)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(max(time.Second, timeout))
	for {
		locked, err := checkpointTryLock(f)
		if err != nil && !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EDEADLK) {
			f.Close()
			return nil, err
		}
		if locked {
			break
		}
		if !time.Now().Before(deadline) {
			f.Close()
			if hint := lockHolderHintText(lockPath); hint != "" {
				return nil, &FileLockTimeoutError{Message: timeoutMessage + "; " + hint}
			}
			return nil, &FileLockTimeoutError{Message: timeoutMessage}
		}
		time.Sleep(50 * time.Millisecond)
	}
	holder.mu.Lock()
	holder.owner, holder.depth = gid, 1
	holder.mu.Unlock()
	if err := f.Truncate(0); err == nil {
		_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
		_ = f.Sync()
	}
	return func() {
		holder.mu.Lock()
		holder.depth = 0
		holder.mu.Unlock()
		if err := f.Truncate(0); err == nil && isWindowsHost {
			_, _ = f.Write([]byte(" "))
		}
		_ = f.Sync()
		f.Close()
	}, nil
}

// Takes the advisory lock file within the timeout and returns its release
func AcquireFileLock(lockPath string, holder *FileLockHolder, timeout time.Duration, msg string) (func(), error) {
	return acquireFileLock(lockPath, holder, timeout, msg)
}

// Live holder hint from the pid a holder stamped into the lock file, empty when there is none
func lockHolderHintText(lockPath string) string {
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		return ""
	}
	raw = []byte(strings.TrimPrefix(decodeUTF8Replace(raw), "\ufeff"))
	fields := strings.FieldsFunc(string(raw), unicode.IsSpace)
	if len(fields) == 0 {
		return ""
	}
	pid, ok := intParse(fields[0])
	if !ok || pid <= 0 || pid == os.Getpid() {
		return ""
	}
	if !isWindowsHost && !syncPidExists(pid) {
		return ""
	}
	return fmt.Sprintf("another icebath process (pid %d) probably still holds it", pid)
}

// Numeric id of the calling goroutine read from its stack header
func lockGoroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	fields := strings.Fields(string(buf[:n]))
	if len(fields) < 2 {
		return 0
	}
	id, _ := strconv.ParseUint(fields[1], 10, 64)
	return id
}
