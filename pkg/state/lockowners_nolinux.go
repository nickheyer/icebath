//go:build !linux

package state

// Only Linux exports /proc/locks, so no holder can be named elsewhere
func writeLockHolders(string) ([]string, error) { return nil, nil }
