package credentials

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Pause between attempts at a lock file another holder has
const lockPoll = 50 * time.Millisecond

// Takes the advisory lock of the lock file at path for this process, waiting until it is free, ctx is done or the timeout passes, and returns its releaser
func lockFile(ctx context.Context, path string, timeout time.Duration) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		return nil, err
	}
	expired := time.NewTimer(timeout)
	defer expired.Stop()
	poll := time.NewTicker(lockPoll)
	defer poll.Stop()
	for {
		locked, err := tryLockFile(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if locked {
			return func() {
				_ = unlockFile(f)
				f.Close()
			}, nil
		}
		select {
		case <-poll.C:
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-expired.C:
			f.Close()
			return nil, fmt.Errorf("timed out after %s waiting for lock file %s: %w", timeout, path, os.ErrDeadlineExceeded)
		}
	}
}
