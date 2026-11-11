package gateway

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/nickheyer/icebath/pkg/activesessions"
)

// Lease or marker whose release the single ordered worker runs
type LeaseReleaser interface{ Release() error }

var (
	leaseIOMu      sync.Mutex
	leaseIOCond    = sync.NewCond(&leaseIOMu)
	leaseIOQueue   []LeaseReleaser
	leaseIOStarted bool
)

// Retires the lease from this process registry view now and queues its durable release on one worker
func ReleaseSoon(lease LeaseReleaser) {
	activesessions.MarkReleaseQueued(lease)
	leaseIOMu.Lock()
	if !leaseIOStarted {
		leaseIOStarted = true
		go leaseIOWorker()
	}
	leaseIOQueue = append(leaseIOQueue, lease)
	leaseIOMu.Unlock()
	leaseIOCond.Signal()
}

// Runs queued releases one at a time in submission order
func leaseIOWorker() {
	for {
		leaseIOMu.Lock()
		for len(leaseIOQueue) == 0 {
			leaseIOCond.Wait()
		}
		lease := leaseIOQueue[0]
		leaseIOQueue = leaseIOQueue[1:]
		leaseIOMu.Unlock()
		releaseQueuedLease(lease)
	}
}

// Releases one lease, a failure logged at debug
func releaseQueuedLease(lease LeaseReleaser) {
	if err := lease.Release(); err != nil {
		slog.Debug(fmt.Sprintf("Failed to release active session slot: %v", err))
	}
}
