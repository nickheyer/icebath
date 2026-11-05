//go:build !windows

package state

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"
	"time"
)

// Errno values flock reports while another process holds the lock
func lockContentionErrno(errno syscall.Errno) bool {
	switch errno {
	case syscall.EAGAIN, syscall.EALREADY, syscall.EINPROGRESS, syscall.EACCES, syscall.EDEADLK:
		return true
	}
	return errno == syscall.EWOULDBLOCK
}

// Signal zero probe, alive when delivery succeeds and gone only when no such process exists
func signalZero(pid int) (alive, gone bool) {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true, false
	}
	return false, errors.Is(err, syscall.ESRCH)
}

// Device and inode of a path through stat
func statFileIdentity(path string) (FileIdentity, bool) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return FileIdentity{}, false
	}
	return FileIdentity{Dev: uint64(st.Dev), Ino: uint64(st.Ino)}, true
}

// Whether the open descriptor still names the file at path
func sameOpenFile(f *os.File, path string) bool {
	var fd, onDisk syscall.Stat_t
	if syscall.Fstat(int(f.Fd()), &fd) != nil || syscall.Stat(path, &onDisk) != nil {
		return false
	}
	return fd.Dev == onDisk.Dev && fd.Ino == onDisk.Ino
}

// Bounded flock acquire that breaks a lock held only by an orphaned descriptor of a provably dead holder
func acquireDBFlock(lockPath string, handle *os.File, timeoutSeconds, pollSeconds float64, description string) (lockOutcome, *os.File, error) {
	deadline := time.Now().Add(time.Duration(timeoutSeconds * float64(time.Second)))
	brokeLock := false
	for {
		if err := flockRetrying(handle, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			if !IsAdvisoryLockContention(err) {
				slog.Warn(fmt.Sprintf("Could not acquire %s %s (%s) — deferring rather than "+
					"waiting out the %.0fs holder timeout on a non-contention error.", description, lockPath, err, timeoutSeconds))
				return lockFailed, handle, nil
			}
			if time.Now().Before(deadline) {
				time.Sleep(time.Duration(pollSeconds * float64(time.Second)))
				continue
			}
			if brokeLock {
				return lockTimedOut, handle, nil
			}
			record := readLockHolderRecord(handle)
			dead, err := lockHolderProvablyDead(record)
			if err != nil {
				return lockTimedOut, handle, err
			}
			if !dead {
				return lockTimedOut, handle, nil
			}
			pid, _ := record.get("pid")
			slog.Warn(fmt.Sprintf("%s %s is held by an orphaned file descriptor (recorded holder pid %s is dead — a "+
				"forked child inherited the lock fd); breaking the stale lock and retaking it on a "+
				"fresh file.", description, lockPath, pyStr(pid)))
			if err := os.Remove(lockPath); err != nil {
				slog.Warn(fmt.Sprintf("Could not break stale %s %s (%s) — deferring.", description, lockPath, err))
				return lockTimedOut, handle, nil
			}
			_ = handle.Close()
			reopened, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
			if err != nil {
				slog.Warn(fmt.Sprintf("Could not break stale %s %s (%s) — deferring.", description, lockPath, err))
				return lockTimedOut, handle, nil
			}
			handle = reopened
			brokeLock = true
			deadline = time.Now().Add(time.Duration(lockBreakReacquireSeconds * float64(time.Second)))
			continue
		}
		if sameOpenFile(handle, lockPath) {
			writeLockHolderRecord(handle)
			return lockAcquired, handle, nil
		}
		_ = handle.Close()
		reopened, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
		if err != nil {
			return lockTimedOut, handle, nil
		}
		handle = reopened
		if !time.Now().Before(deadline) {
			return lockTimedOut, handle, nil
		}
	}
}

// Takes the FTS rebuild authority through flock
func acquireAdmissionLock(lockPath string, handle *os.File, timeout float64) (lockOutcome, *os.File, error) {
	return acquireDBFlock(lockPath, handle, timeout, ftsRebuildLockPollSeconds, "FTS rebuild lock")
}

// Clears the holder record and drops the flock, ignoring IO failures
func releaseAdmissionLock(handle *os.File) {
	clearLockHolderRecord(handle)
	_ = flockRetrying(handle, syscall.LOCK_UN)
}

// flock retried across signal interruptions the way the Python wrapper retries EINTR
func flockRetrying(f *os.File, how int) error {
	for {
		if err := syscall.Flock(int(f.Fd()), how); !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
