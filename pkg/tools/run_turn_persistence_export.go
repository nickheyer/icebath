package tools

// Detaches the pending check interval watchers under the registry lock so concurrent appends are kept
func (r *ProcessRegistry) TakePendingWatchers() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	watchers := r.PendingWatchers
	r.PendingWatchers = []map[string]any{}
	return watchers
}
