package state

// Byte range one guarded inode is locked over
type lockRange struct{ start, length int64 }

// Inode a handle guards mapped to its locked range
type lockGuardHeld map[FileIdentity]lockRange

// Windows has no POSIX advisory locks, so the guard never arms
func lockGuardSupported() bool { return false }

// Returns the record unchanged, the guard being a no-op without POSIX locks
func lockGuardHold(dbPath string, held lockGuardHeld) lockGuardHeld {
	if held == nil {
		held = lockGuardHeld{}
	}
	return held
}

// Nothing to drop without POSIX locks
func lockGuardRelease(held lockGuardHeld) {}
