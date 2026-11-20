//go:build unix && !linux && !darwin

package state

// No OFD lock command on this kernel, so the guard stays a no-op
func platformLockGuard() *lockGuardOps { return nil }
