package fsutil

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	syncutil "github.com/nickheyer/icebath/pkg/utils/sync"
)

// Pause between attempts at a lock file another process holds
const lockPoll = 50 * time.Millisecond

// Advisory lock on one lock file, shared by every goroutine and process that takes it, which the goroutine holding it may take again; the holder's pid is stamped in the file after one pad byte while held, so other processes can read it past the byte a Windows lock covers
type FileLock struct {
	path string
	sem  chan struct{}
	// Guards owner, depth and file together, so a nested Lock and the release of the outermost hold never interleave
	mu    sync.Mutex
	owner uint64
	depth int
	file  *os.File
}

var fileLocks sync.Map

// The FileLock of the lock file at path, the same value for every caller in this process naming the same file
func SharedFileLock(path string) *FileLock {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	key := pathutil.FoldCase(path)
	if l, ok := fileLocks.Load(key); ok {
		return l.(*FileLock)
	}
	l, _ := fileLocks.LoadOrStore(key, &FileLock{path: path, sem: make(chan struct{}, 1)})
	return l.(*FileLock)
}

// Takes the lock for the calling goroutine, at once when it already holds it, else waiting for other goroutines and processes until it is free, ctx is done or a positive timeout passes; call release once for every successful Lock
func (l *FileLock) Lock(ctx context.Context, timeout time.Duration) (release func(), err error) {
	id := syncutil.GoroutineID()
	l.mu.Lock()
	if id != 0 && l.owner == id {
		l.depth++
		l.mu.Unlock()
		return l.releaser(), nil
	}
	l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var expired <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case l.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-expired:
		return nil, l.timeoutError(timeout)
	}
	f, err := l.lockFile(ctx, expired, timeout)
	if err != nil {
		<-l.sem
		return nil, err
	}
	l.mu.Lock()
	l.file, l.depth, l.owner = f, 1, id
	l.mu.Unlock()
	return l.releaser(), nil
}

// Opens the lock file, polls for its kernel lock and stamps this process's pid into it after a pad byte
func (l *FileLock) lockFile(ctx context.Context, expired <-chan time.Time, timeout time.Duration) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o777); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(l.path, os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		return nil, err
	}
	var poll *time.Ticker
	for {
		ok, err := TryLockFile(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if ok {
			break
		}
		if poll == nil {
			poll = time.NewTicker(lockPoll)
			defer poll.Stop()
		}
		select {
		case <-poll.C:
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-expired:
			f.Close()
			return nil, l.timeoutError(timeout)
		}
	}
	if f.Truncate(0) == nil {
		_, _ = f.WriteAt([]byte(" "+strconv.Itoa(os.Getpid())+"\n"), 0)
		_ = f.Sync()
	}
	return f, nil
}

// Release of one hold, doing nothing when called again
func (l *FileLock) releaser() func() {
	var once sync.Once
	return func() { once.Do(l.release) }
}

func (l *FileLock) release() {
	l.mu.Lock()
	l.depth--
	if l.depth > 0 {
		l.mu.Unlock()
		return
	}
	f := l.file
	l.file, l.owner = nil, 0
	l.mu.Unlock()
	if f.Truncate(0) == nil {
		_ = f.Sync()
	}
	_ = UnlockFile(f)
	f.Close()
	<-l.sem
}

func (l *FileLock) timeoutError(timeout time.Duration) error {
	return &LockTimeoutError{Path: l.path, Timeout: timeout, HolderPID: holderPID(l.path)}
}

// Pid another live process stamped into a lock file after its pad byte, zero when there is none; reading from offset one keeps clear of the byte a Windows holder has locked
func holderPID(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	var buf [32]byte
	n, _ := f.ReadAt(buf[:], 1)
	fields := bytes.Fields(buf[:n])
	if len(fields) == 0 {
		return 0
	}
	pid, err := strconv.Atoi(string(fields[0]))
	if err != nil || pid <= 0 || pid == os.Getpid() || processGone(pid) {
		return 0
	}
	return pid
}

// Lock wait that gave up once its timeout passed, with the pid another live process stamped into the lock file, zero when unknown
type LockTimeoutError struct {
	Path      string
	Timeout   time.Duration
	HolderPID int
}

// The lock file, the timeout and the likely holder, as in timed out after 15s waiting for lock file /h/.env.lock; process 4242 probably holds it
func (e *LockTimeoutError) Error() string {
	msg := fmt.Sprintf("timed out after %s waiting for lock file %s", e.Timeout, e.Path)
	if e.HolderPID > 0 {
		msg += fmt.Sprintf("; process %d probably holds it", e.HolderPID)
	}
	return msg
}

// Lets errors.Is match os.ErrDeadlineExceeded
func (e *LockTimeoutError) Unwrap() error { return os.ErrDeadlineExceeded }
