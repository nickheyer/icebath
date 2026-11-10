package pyfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Python TimeoutError a holder lock waiter raises once its deadline passes
type LockTimeoutError struct{ Message string }

func (e *LockTimeoutError) Error() string { return e.Message }

// Per goroutine reentrancy depth of one advisory lock, the threading.local holder hermes locks share
type LockHolder struct{ depth pythread.Local }

// Depth the calling goroutine holds the lock at, zero when it does not hold it
func (h *LockHolder) Depth() int {
	v, _ := h.depth.Get()
	n, _ := v.(int)
	return n
}

// Takes the cross process advisory lock at lockPath, reentrant per goroutine, failing with the message at the deadline
func HolderFileLock(lockPath string, holder *LockHolder, timeout time.Duration, timeoutMessage string) (func(), error) {
	if d := holder.Depth(); d > 0 {
		holder.depth.Set(d + 1)
		return func() { holder.depth.Set(holder.Depth() - 1) }, nil
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o777); err != nil {
		return nil, err
	}
	f, err := openHolderLockFile(lockPath)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(max(time.Second, timeout))
	for {
		err := holderKernelLock(f, true)
		if err == nil {
			break
		}
		if !isHolderLockContention(err) {
			f.Close()
			return nil, err
		}
		if !time.Now().Before(deadline) {
			f.Close()
			if hint := LockHolderHint(lockPath); hint != "" {
				return nil, &LockTimeoutError{Message: timeoutMessage + "; " + hint}
			}
			return nil, &LockTimeoutError{Message: timeoutMessage}
		}
		time.Sleep(50 * time.Millisecond)
	}
	holder.depth.Set(1)
	if f.Truncate(0) == nil {
		fmt.Fprintf(f, "%d\n", os.Getpid())
		f.Sync()
	}
	return func() {
		holder.depth.Set(0)
		if f.Truncate(0) == nil {
			if holderReleasePad != "" {
				f.WriteString(holderReleasePad)
			}
			f.Sync()
		}
		holderKernelLock(f, false)
		f.Close()
	}, nil
}

// Live holder hint from the pid a holder stamps into the lock file, empty for none, a dead pid or this process
func LockHolderHint(lockPath string) string {
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		return ""
	}
	fields := strings.Fields(strings.ToValidUTF8(strings.TrimPrefix(string(raw), "\uFEFF"), "\uFFFD"))
	if len(fields) == 0 {
		return ""
	}
	n, ok := pyint.Parse(fields[0])
	if !ok || !n.IsInt64() {
		return ""
	}
	pid := n.Int64()
	if pid <= 0 || pid == int64(os.Getpid()) {
		return ""
	}
	if holderGone(int(pid)) {
		return ""
	}
	return fmt.Sprintf("another icebath process (pid %d) probably still holds it", pid)
}
