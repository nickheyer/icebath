package tools

import (
	"sync"
	"time"
)

// Teardown workers in flight, joined at shutdown
var teardownWorkers = struct {
	sync.Mutex
	pending map[chan struct{}]bool
}{pending: map[chan struct{}]bool{}}

// Runs a teardown on its own worker, the channel closing when it finishes
func startTeardown(work func()) chan struct{} {
	done := make(chan struct{})
	teardownWorkers.Lock()
	teardownWorkers.pending[done] = true
	teardownWorkers.Unlock()
	go func() {
		defer func() {
			close(done)
			teardownWorkers.Lock()
			delete(teardownWorkers.pending, done)
			teardownWorkers.Unlock()
		}()
		work()
	}()
	return done
}

// WaitForAllBackendTeardowns joins every in flight teardown worker within the timeout
func WaitForAllBackendTeardowns(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		teardownWorkers.Lock()
		var first chan struct{}
		for ch := range teardownWorkers.pending {
			first = ch
			break
		}
		teardownWorkers.Unlock()
		if first == nil {
			return true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		select {
		case <-first:
		case <-time.After(remaining):
			return false
		}
	}
}
