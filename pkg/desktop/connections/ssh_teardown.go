package connections

import (
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

const (
	sshTeardownGrace = 6 * time.Second
	sshTeardownForce = time.Second
)

// Waits for every task to settle or the timeout, whichever comes first
func waitForTeardown(tasks []func(), timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for _, task := range tasks {
			wg.Add(1)
			go func() {
				defer wg.Done()
				task()
			}()
		}
		wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

type teardownEntry struct {
	done chan struct{}
	ssh  backend.SSHTransport
}

// SSHTeardownTracker keeps transports removed from routing until their remote kill settles
type SSHTeardownTracker struct {
	mu      sync.Mutex
	pending map[*teardownEntry]bool
}

// NewSSHTeardownTracker is an empty tracker
func NewSSHTeardownTracker() *SSHTeardownTracker {
	return &SSHTeardownTracker{pending: map[*teardownEntry]bool{}}
}

// Whether a teardown is still settling
func (t *SSHTeardownTracker) HasPending() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending) > 0
}

// Holds the transport for a quit force close until the returned release runs
func (t *SSHTeardownTracker) Begin(ssh backend.SSHTransport) func() {
	entry := &teardownEntry{done: make(chan struct{}), ssh: ssh}
	t.mu.Lock()
	t.pending[entry] = true
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		delete(t.pending, entry)
		t.mu.Unlock()
		close(entry.done)
	}
}

// Waits the grace for pending teardowns and bootstraps, then force cleans and closes every held transport within the bound
func (t *SSHTeardownTracker) Finish(bootstraps []*backend.Future[backend.Connection], forceCleanup func()) {
	t.mu.Lock()
	var tasks []func()
	transports := map[backend.SSHTransport]bool{}
	for entry := range t.pending {
		done := entry.done
		tasks = append(tasks, func() { <-done })
		transports[entry.ssh] = true
	}
	t.mu.Unlock()
	for _, f := range bootstraps {
		tasks = append(tasks, func() { _, _ = f.Wait() })
	}
	waitForTeardown(tasks, sshTeardownGrace)
	force := []func(){forceCleanup}
	for ssh := range transports {
		force = append(force, func() { _ = ssh.Close() })
	}
	waitForTeardown(force, sshTeardownForce)
}
