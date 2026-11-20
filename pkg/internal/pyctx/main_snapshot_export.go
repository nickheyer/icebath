package pyctx

// Returns the restore of the main thread context to the values it holds now
func SnapshotMainForTests() (restore func()) {
	saved := root.Copy()
	return func() {
		root.mu.Lock()
		root.vals, root.keys = saved.vals, saved.keys
		root.mu.Unlock()
	}
}
