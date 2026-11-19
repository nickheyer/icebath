package gatewayrun

import (
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Goroutines of the runner that write into a profile home, joined before a turn or the runner ends
type homeWriters struct {
	mu   sync.Mutex
	live int
	idle chan struct{}
}

// Runs fn on its own goroutine counted until it returns
func (w *homeWriters) Go(fn func()) {
	w.mu.Lock()
	if w.live == 0 {
		w.idle = make(chan struct{})
	}
	w.live++
	w.mu.Unlock()
	go func() {
		defer w.done()
		fn()
	}()
}

func (w *homeWriters) done() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.live--
	if w.live == 0 {
		close(w.idle)
	}
}

// Blocks until no counted goroutine is running
func (w *homeWriters) Wait() {
	w.mu.Lock()
	live, idle := w.live, w.idle
	w.mu.Unlock()
	if live != 0 {
		<-idle
	}
}

// Waits for the registry releases and runtime status writes a turn end queued
func settleTurnEndWrites() {
	gateway.AwaitLeaseReleases()
	gateway.SettleRuntimeStatus()
}

// Waits for the agent releases and memory commits the runner spawned, then for the turn end writes
func (r *GatewayRunner) settleHomeWrites() {
	r.releaseWriters.Wait()
	settleTurnEndWrites()
}
