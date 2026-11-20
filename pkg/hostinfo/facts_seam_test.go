package hostinfo

// ClearCaches clears every cached host fact
func ClearCaches() {
	for _, reset := range cacheResets {
		reset()
	}
}
