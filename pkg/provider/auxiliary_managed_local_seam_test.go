package provider

import "time"

// Drops the cached managed endpoint so the next read consults the state file
func auxResetManagedLocalCache() {
	auxManagedLocalMu.Lock()
	auxManagedLocalCacheAt, auxManagedLocalCached = time.Time{}, ""
	auxManagedLocalMu.Unlock()
}
