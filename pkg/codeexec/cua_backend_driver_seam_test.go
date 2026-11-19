package codeexec

// Clears the single entry no overlay probe cache
func clearNoOverlayCache() {
	noOverlayCache.Lock()
	noOverlayCache.valid = false
	noOverlayCache.Unlock()
}
