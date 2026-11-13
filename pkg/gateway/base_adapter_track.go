package gateway

// Tracks a detached task for cancellation at shutdown until it finishes, for callers outside this package
func (b *BasePlatformAdapter) TrackBackgroundTask(task *AdapterTask) { b.trackBackgroundTask(task) }

// Tracked detached tasks that have not finished yet
func (b *BasePlatformAdapter) BackgroundTasks() []*AdapterTask {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*AdapterTask, 0, len(b.backgroundTasks))
	for task := range b.backgroundTasks {
		out = append(out, task)
	}
	return out
}
