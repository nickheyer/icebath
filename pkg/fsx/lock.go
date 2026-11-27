package fsx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Pause between attempts at a lock file another holder has
const lockPoll = 50 * time.Millisecond

// Takes the advisory lock of the lock file at path, made with its folder when missing and never opened through a link, without waiting:
// its release, or false while another process or handle holds it. The file itself is never removed, so a process waiting on it never holds
// the lock of a file gone from its folder
func TryLock(path string) (release func(), ok bool, err error) {
	f, err := openLock(path)
	if err != nil {
		return nil, false, err
	}
	if ok, err = tryLock(f); err != nil || !ok {
		f.Close()
		return nil, false, err
	}
	return hold(f), true, nil
}

// Takes the advisory lock of the lock file at path, made with its folder when missing, waiting while another process or handle holds it
// until ctx is done or the timeout passes: a zero timeout tries once, a negative one waits for as long as it takes
func Lock(ctx context.Context, path string, timeout time.Duration) (release func(), err error) {
	f, err := openLock(path)
	if err != nil {
		return nil, err
	}
	var expired <-chan time.Time
	if timeout >= 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		expired = timer.C
	}
	var poll *time.Ticker
	for {
		ok, err := tryLock(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if ok {
			return hold(f), nil
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
			return nil, &TimeoutError{Path: path, Timeout: timeout, Holder: holder(path)}
		}
	}
}

// Opens the file at path, made with its folder when missing, holding its advisory lock until the file is closed: waiting for the lock
// when wait is true, else false while another process or handle holds it. For a lock file that keeps data of its own, which TryLock and
// Lock would overwrite
func OpenLocked(path string, wait bool) (*os.File, bool, error) {
	f, err := openLock(path)
	if err != nil {
		return nil, false, err
	}
	for {
		ok, err := tryLock(f)
		switch {
		case err != nil:
			f.Close()
			return nil, false, err
		case ok:
			return f, true, nil
		case !wait:
			f.Close()
			return nil, false, nil
		}
		time.Sleep(lockPoll)
	}
}

func openLock(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|noFollow, 0o666)
}

// Stamps this process's pid in the locked file after a pad byte, so a process waiting on it can name the holder, and answers the release,
// which does nothing when called again
func hold(f *os.File) func() {
	if f.Truncate(0) == nil {
		_, _ = f.WriteAt([]byte(" "+strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = f.Truncate(0)
			_ = unlock(f)
			f.Close()
		})
	}
}

// Pid another live process stamped in a lock file, zero when there is none; reading from offset one keeps clear of the byte a Windows
// holder has locked
func holder(path string) int {
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

// Lock wait that gave up once its timeout passed, with the pid another live process stamped in the lock file, zero when unknown
type TimeoutError struct {
	Path    string
	Timeout time.Duration
	Holder  int
}

func (e *TimeoutError) Error() string {
	msg := fmt.Sprintf("timed out after %s waiting for lock file %s", e.Timeout, e.Path)
	if e.Holder > 0 {
		msg += fmt.Sprintf("; process %d probably holds it", e.Holder)
	}
	return msg
}

// Lets errors.Is match os.ErrDeadlineExceeded
func (e *TimeoutError) Unwrap() error { return os.ErrDeadlineExceeded }
