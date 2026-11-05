//go:build unix

package state

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"syscall"
)

// SQLite unix VFS lock geometry, the SHARED range of the main file and the shm deadman byte
const (
	pendingByte = 0x40000000
	sharedFirst = pendingByte + 2
	sharedSize  = 510
	shmDMSByte  = 128
)

// One fcntl record lock request on fd, whence SEEK_SET
type flockFunc func(fd, cmd int, lockType int16, start, length int64) error

// The fcntl capability the guard drives, absent when the platform has no OFD lock command
type lockGuardOps struct {
	ofdSetLK, setLK int
	rdlck, unlck    int16
	fcntl           flockFunc
}

// Guard capability from an OFD command and an fcntl callable, nil when either is missing
func newLockGuardOps(ofdSetLK *int, setLK int, rdlck, unlck int16, fcntl flockFunc) *lockGuardOps {
	if ofdSetLK == nil || fcntl == nil {
		return nil
	}
	return &lockGuardOps{ofdSetLK: *ofdSetLK, setLK: setLK, rdlck: rdlck, unlck: unlck, fcntl: fcntl}
}

// Byte range one guarded inode is locked over
type lockRange struct{ start, length int64 }

// Inode a handle guards mapped to its locked range
type lockGuardHeld map[FileIdentity]lockRange

var (
	lockGuard        = platformLockGuard()
	lockGuardMu      sync.Mutex
	lockGuardHandles = map[FileIdentity]int{}
)

// Whether the platform offers open file description locks
func lockGuardSupported() bool { return lockGuard != nil }

// Non blocking OFD lock, or a process owned POSIX lock when posix is set, false when held exclusive elsewhere
func ofdLock(fd int, lockType int16, start, length int64, posix bool) (bool, error) {
	cmd := lockGuard.ofdSetLK
	if posix {
		cmd = lockGuard.setLK
	}
	err := lockGuard.fcntl(fd, cmd, lockType, start, length)
	if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EALREADY) || errors.Is(err, syscall.EINPROGRESS) {
		return false, nil
	}
	return err == nil, err
}

// One descriptor of this process and the inode it references
type ownFD struct {
	fd    int
	ident FileIdentity
}

// Descriptors of this process on one of identities, from the first listable fd directory
func ownFDsFor(identities map[FileIdentity]lockRange) []ownFD {
	for _, dir := range []string{"/proc/self/fd", "/dev/fd"} {
		names, err := listdirUnordered(dir)
		if err != nil {
			continue
		}
		var out []ownFD
		for _, name := range names {
			if !pyIsDigits(name) {
				continue
			}
			n, err := pyIntOf(name)
			if err != nil || !n.IsInt64() {
				continue
			}
			var st syscall.Stat_t
			if syscall.Fstat(int(n.Int64()), &st) != nil {
				continue
			}
			ident := FileIdentity{Dev: uint64(st.Dev), Ino: uint64(st.Ino)}
			if _, ok := identities[ident]; ok {
				out = append(out, ownFD{int(n.Int64()), ident})
			}
		}
		return out
	}
	return nil
}

// Guard ranges of the main file and the shm sidecar that exist, keyed by inode
func guardRanges(dbPath string) lockGuardHeld {
	ranges := lockGuardHeld{}
	for path, rng := range map[string]lockRange{dbPath: {sharedFirst, sharedSize}, dbPath + "-shm": {shmDMSByte, 1}} {
		if ident, err := statIdentity(path); err == nil {
			ranges[ident] = rng
		}
	}
	return ranges
}

// Locks the guard ranges on every descriptor this process holds on the store, counting each inode once per handle
func lockGuardHold(dbPath string, held lockGuardHeld) lockGuardHeld {
	if held == nil {
		held = lockGuardHeld{}
	}
	if !lockGuardSupported() {
		return held
	}
	ranges := guardRanges(dbPath)
	lockGuardMu.Lock()
	defer lockGuardMu.Unlock()
	for _, own := range ownFDsFor(ranges) {
		rng := ranges[own.ident]
		ok, err := ofdLock(own.fd, lockGuard.rdlck, rng.start, rng.length, false)
		if err != nil {
			slog.Debug(fmt.Sprintf("WAL lock guard unavailable for %s", dbPath), "error", err)
			break
		}
		if _, have := held[own.ident]; ok && !have {
			held[own.ident] = rng
			lockGuardHandles[own.ident]++
		}
	}
	return held
}

// Drops this handle's claim, the last handle per inode swapping the OFD lock back to a POSIX one and unlocking
func lockGuardRelease(held lockGuardHeld) {
	if !lockGuardSupported() || len(held) == 0 {
		return
	}
	lockGuardMu.Lock()
	defer lockGuardMu.Unlock()
	toUnlock := lockGuardHeld{}
	for ident, rng := range held {
		remaining := 0
		if n, ok := lockGuardHandles[ident]; ok {
			remaining = n - 1
		}
		if remaining > 0 {
			lockGuardHandles[ident] = remaining
		} else {
			delete(lockGuardHandles, ident)
			toUnlock[ident] = rng
		}
	}
	clear(held)
	if len(toUnlock) == 0 {
		return
	}
	for _, own := range ownFDsFor(toUnlock) {
		rng := toUnlock[own.ident]
		if _, err := ofdLock(own.fd, lockGuard.rdlck, rng.start, rng.length, true); err != nil {
			return
		}
		if _, err := ofdLock(own.fd, lockGuard.unlck, rng.start, rng.length, false); err != nil {
			return
		}
	}
}
