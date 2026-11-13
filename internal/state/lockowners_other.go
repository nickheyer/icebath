//go:build !linux

package state

// Only Linux exports which process holds which lock, so no owner can be named here
func readLockOwners(map[fileIdentity]string) ([]lockOwner, bool) { return nil, false }
