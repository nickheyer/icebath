package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileLockTimeoutError is the TimeoutError a waiter raises once the deadline passes with another holder on the lock
type FileLockTimeoutError struct {
	Message string
}

func (e *FileLockTimeoutError) Error() string { return e.Message }

// Per goroutine reentrancy depth of one lock path, the threading.local holder of the hermes lock
type fileLockHolder struct {
	mu    sync.Mutex
	depth map[uint64]int
}

var fileLockHolders sync.Map

// Holder tracking reentrancy for the resolved lock path
func fileLockHolderFor(lockPath string) *fileLockHolder {
	h, _ := fileLockHolders.LoadOrStore(resolvedKey(lockPath), &fileLockHolder{depth: map[uint64]int{}})
	return h.(*fileLockHolder)
}

// Bumps the calling goroutine's depth when it already holds the lock and returns the matching unwind
func (h *fileLockHolder) reenter(gid uint64) (func(), bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.depth[gid] == 0 {
		return nil, false
	}
	h.depth[gid]++
	return func() {
		h.mu.Lock()
		h.depth[gid]--
		h.mu.Unlock()
	}, true
}

// Records the calling goroutine's depth, one after acquiring and zero on release
func (h *fileLockHolder) set(gid uint64, depth int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if depth == 0 {
		delete(h.depth, gid)
		return
	}
	h.depth[gid] = depth
}

// FileLock takes the cross process advisory lock at the path, reentrant per goroutine, failing with the message at the deadline
func FileLock(lockPath string, timeout time.Duration, timeoutMessage string) (release func(), err error) {
	holder := fileLockHolderFor(lockPath)
	gid := goroutineID()
	if unwind, nested := holder.reenter(gid); nested {
		return unwind, nil
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o777); err != nil {
		return nil, err
	}
	f, err := openLockFile(lockPath)
	if err != nil {
		return nil, err
	}
	if timeout < time.Second {
		timeout = time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		err := kernelLock(f, true)
		if err == nil {
			break
		}
		if !isLockContention(err) {
			f.Close()
			return nil, err
		}
		if !time.Now().Before(deadline) {
			f.Close()
			if hint := lockHolderHint(lockPath); hint != "" {
				return nil, &FileLockTimeoutError{Message: timeoutMessage + "; " + hint}
			}
			return nil, &FileLockTimeoutError{Message: timeoutMessage}
		}
		time.Sleep(50 * time.Millisecond)
	}
	holder.set(gid, 1)
	if f.Truncate(0) == nil {
		fmt.Fprintf(f, "%d\n", os.Getpid())
		f.Sync()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			holder.set(gid, 0)
			if f.Truncate(0) == nil {
				if lockReleasePad != "" {
					f.WriteString(lockReleasePad)
				}
				f.Sync()
			}
			kernelLock(f, false)
			f.Close()
		})
	}, nil
}
