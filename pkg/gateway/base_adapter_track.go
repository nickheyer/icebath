package gateway

// Tracks a detached task for cancellation at shutdown until it finishes, for callers outside this package
func (b *BasePlatformAdapter) TrackBackgroundTask(task *AdapterTask) { b.trackBackgroundTask(task) }
