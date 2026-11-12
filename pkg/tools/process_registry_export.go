package tools

import "context"

// Empties the running and finished tables and the completion queue the way hermes registry fixtures do, returning the restore
func IsolateProcessRegistryForTests(r *ProcessRegistry) (restore func()) {
	drain := func() {
		for {
			if _, ok := r.CompletionQueue.GetNowait(); !ok {
				return
			}
		}
	}
	r.mu.Lock()
	savedRunning, savedFinished := r.running, r.finished
	r.running, r.finished = map[string]*ProcessSession{}, map[string]*ProcessSession{}
	r.mu.Unlock()
	drain()
	return func() {
		drain()
		r.mu.Lock()
		r.running, r.finished = savedRunning, savedFinished
		r.mu.Unlock()
	}
}

// Records an exit and publishes the completion the way hermes cases call mark_exited then _move_to_finished
func FinishProcessForTests(ctx context.Context, r *ProcessRegistry, s *ProcessSession, exitCode int) {
	s.markExited(&exitCode, "exited", "")
	r.moveToFinished(ctx, s, false)
}
