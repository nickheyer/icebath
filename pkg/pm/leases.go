package pm

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// InstallLockTimeout is how long a reader of install state waits for a writer before skipping
const InstallLockTimeout = 10 * time.Second

// Holds the per install dependency lock, held false when the wait expired
func RuntimeLock(ctx context.Context, project string, timeout time.Duration) (held bool, release func(), err error) {
	lock := filepath.Join(InstallStateDir(ctx, project), ".install.lock")
	f, err := LockPath(lock, timeout, true)
	if err != nil {
		return false, nil, err
	}
	if f == nil {
		slog.Warn(fmt.Sprintf("dependency lock still held after %gs; continuing without it (%s)", timeout.Seconds(), lock))
		return false, func() {}, nil
	}
	return true, func() { f.Close() }, nil
}

var (
	leasesMu   sync.Mutex
	liveLeases = map[*os.File]string{}
)

// Takes a lease file's byte lock, waiting or trying once
var leaseLock = LockFD

// Unlinks a lease file
var unlinkLease = os.Remove

// Pins a generation's parent directory for this process, the returned release letting go early
func LeaseGeneration(environment string) (func(), error) {
	return LeaseDirectory(filepath.Dir(environment))
}

// Pins a lease managed generation directory until release or process exit
func LeaseDirectory(generation string) (func(), error) {
	if !isRegularFile(filepath.Join(generation, ".lease-managed")) {
		return func() {}, nil
	}
	leases := filepath.Join(generation, ".leases")
	if err := os.Mkdir(leases, 0o777); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	if _, err := pruneUnlockedLeases(leases); err != nil {
		return nil, err
	}
	var f *os.File
	var lease string
	for {
		lease = filepath.Join(leases, randomHex32())
		var err error
		f, err = os.OpenFile(lease, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		if _, err := leaseLock(f, true, WaitForever); err != nil {
			f.Close()
			return nil, err
		}
		if _, err := os.Stat(lease); err == nil {
			break
		}
		f.Close()
	}
	leasesMu.Lock()
	liveLeases[f] = lease
	leasesMu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { releaseLease(f) }) }, nil
}

// Closes one lease and removes its file
func releaseLease(f *os.File) {
	leasesMu.Lock()
	lease, ok := liveLeases[f]
	delete(liveLeases, f)
	leasesMu.Unlock()
	if !ok {
		return
	}
	f.Close()
	_ = unlinkLease(lease)
}

// Releases every lease this process still holds, for the binary to call as it exits
func ReleaseAllLeases() {
	leasesMu.Lock()
	files := make([]*os.File, 0, len(liveLeases))
	for f := range liveLeases {
		files = append(files, f)
	}
	leasesMu.Unlock()
	for _, f := range files {
		releaseLease(f)
	}
}

// Reports whether any process still holds a lease on the generation
func LeasesHeld(generation string) (bool, error) {
	return pruneUnlockedLeases(filepath.Join(generation, ".leases"))
}

// Removes abandoned lease files and reports whether a live lock remains, failing closed on access errors
func pruneUnlockedLeases(leases string) (bool, error) {
	entries, err := filepath.Glob(filepath.Join(globEscape(leases), "*"))
	if err != nil {
		return false, err
	}
	held := false
	for _, lease := range entries {
		f, err := openLeaseProbe(lease)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			held = true
			continue
		}
		locked, err := leaseLock(f, false, 0)
		if err != nil {
			f.Close()
			return false, err
		}
		if !locked {
			held = true
		} else if err := unlinkLease(lease); err != nil && !errors.Is(err, fs.ErrNotExist) {
			held = true
		}
		f.Close()
	}
	return held, nil
}

// Escapes glob metacharacters in a literal directory path
func globEscape(path string) string {
	var out []rune
	for _, r := range path {
		switch r {
		case '*', '?', '[', ']':
			out = append(out, '[', r, ']')
		default:
			out = append(out, r)
		}
	}
	return string(out)
}
