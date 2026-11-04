package provider

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	providerModelsCacheThreadLock sync.Mutex
	providerModelsCacheLockWait   = 10 * time.Second
)

var modelsLog = slog.Default()

// Takes the thread lock plus the cross process lock beside the provider models cache file
func providerModelsCacheLock(path string) (func(), error) {
	lockPath := filepath.Join(filepath.Dir(path), filepath.Base(path)+".lock")
	providerModelsCacheThreadLock.Lock()
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		providerModelsCacheThreadLock.Unlock()
		return nil, err
	}
	f, err := openLockFile(lockPath)
	if err != nil {
		providerModelsCacheThreadLock.Unlock()
		return nil, err
	}
	deadline := time.Now().Add(providerModelsCacheLockWait)
	for {
		err := kernelLock(f, true)
		if err == nil {
			break
		}
		if !isLockContention(err) {
			f.Close()
			providerModelsCacheThreadLock.Unlock()
			return nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			providerModelsCacheThreadLock.Unlock()
			return nil, &LockTimeoutError{LockPath: lockPath, Hint: "Timed out waiting for the provider models cache lock (" + path + ")"}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return func() {
		kernelLock(f, false)
		f.Close()
		providerModelsCacheThreadLock.Unlock()
	}, nil
}

// Runs a reread, set one row, save cycle under both locks, skipping it when the lock cannot be taken
func writeUnderCacheLock(path string, write func()) {
	release, err := providerModelsCacheLock(path)
	if err != nil {
		modelsLog.Debug("provider models cache write skipped", "path", path, "error", err)
		return
	}
	defer release()
	write()
}
