//go:build linux || darwin

package state

import (
	"errors"
	"fmt"
	"log/slog"

	"golang.org/x/sys/unix"
	"modernc.org/sqlite"
)

// Guard ranges of the main file and shm sidecar, the main file skipped when SQLite already locks it by OFD
func guardRanges(path string) map[fileIdentity]lockRange {
	ranges := map[fileIdentity]lockRange{}
	if !sqlite.OFDLockingEnabled() {
		if id, err := identityOf(path); err == nil {
			ranges[id] = lockRange{sharedFirst, sharedSize}
		}
	}
	if id, err := identityOf(path + shmSuffix); err == nil {
		ranges[id] = lockRange{shmDMSByte, 1}
	}
	return ranges
}

// Non blocking OFD lock on fd, a process POSIX lock when posix is set, false when held exclusive elsewhere
func ofdLock(fd int, kind int16, r lockRange, posix bool) (bool, error) {
	cmd := ofdSetLK
	if posix {
		cmd = unix.F_SETLK
	}
	lk := unix.Flock_t{Type: kind, Whence: 0, Start: r.start, Len: r.length}
	err := unix.FcntlFlock(uintptr(fd), cmd, &lk)
	if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EALREADY) || errors.Is(err, unix.EINPROGRESS) {
		return false, nil
	}
	return err == nil, err
}

// Copies the guard ranges as OFD locks onto every descriptor this process holds on the store, extending held
func (e *Engine) guardHold(path string, held guardHeld) guardHeld {
	if held == nil {
		held = guardHeld{}
	}
	ranges := guardRanges(path)
	e.guards.mu.Lock()
	defer e.guards.mu.Unlock()
	fds, _ := ownDescriptors()
	for _, d := range fds {
		r, ok := ranges[d.id]
		if !ok {
			continue
		}
		locked, err := ofdLock(d.fd, unix.F_RDLCK, r, false)
		if err != nil {
			slog.Debug(fmt.Sprintf("WAL lock guard unavailable for %s: %v", path, err))
			break
		}
		if _, have := held[d.id]; locked && !have {
			held[d.id] = r
			e.guards.handles[d.id]++
		}
	}
	return held
}

// Drops one claim, the last one on an inode swapping the OFD copy back for the POSIX lock SQLite expects
func (e *Engine) guardRelease(held guardHeld) {
	if len(held) == 0 {
		return
	}
	e.guards.mu.Lock()
	defer e.guards.mu.Unlock()
	unlock := guardHeld{}
	for id, r := range held {
		remaining, ok := e.guards.handles[id]
		if !ok {
			remaining = 1
		}
		if remaining--; remaining > 0 {
			e.guards.handles[id] = remaining
		} else {
			delete(e.guards.handles, id)
			unlock[id] = r
		}
	}
	clear(held)
	if len(unlock) == 0 {
		return
	}
	fds, _ := ownDescriptors()
	for _, d := range fds {
		r, ok := unlock[d.id]
		if !ok {
			continue
		}
		if _, err := ofdLock(d.fd, unix.F_RDLCK, r, true); err != nil {
			return
		}
		if _, err := ofdLock(d.fd, unix.F_UNLCK, r, false); err != nil {
			return
		}
	}
}
