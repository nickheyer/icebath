package gatewayrun

import (
	"context"
	"math"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Threads of the separate pool for session housekeeping chores
const housekeepingMaxWorkers = 4

// Blocking work on the turn pool, running in a copy of the caller context on the worker thread
func (r *GatewayRunner) runInExecutorWithContext(ctx context.Context, fn func(ctx context.Context) (any, error)) (any, error) {
	executor, err := r.getExecutor()
	if err != nil {
		return nil, err
	}
	fut, err := executor.submit(func(workerCtx context.Context) (any, error) { return fn(onExecutorThread(ctx, workerCtx)) })
	if err != nil {
		return nil, err
	}
	return fut.await(ctx)
}

// Housekeeping work on its own bounded pool so abandoned work never occupies the turn pool
func (r *GatewayRunner) runHousekeepingInExecutor(ctx context.Context, fn func(ctx context.Context) (any, error)) (any, error) {
	executor, err := r.getHousekeepingExecutor()
	if err != nil {
		return nil, err
	}
	fut, err := executor.submit(func(workerCtx context.Context) (any, error) { return fn(onExecutorThread(ctx, workerCtx)) })
	if err != nil {
		return nil, err
	}
	return fut.await(ctx)
}

// Pool held at slot, created under the executor lock when absent or shut down, refused while closing
func (r *GatewayRunner) getOrCreatePool(slot *gatewayExecutor, makePool func() gatewayExecutor) (gatewayExecutor, error) {
	if r.executorLock == nil {
		r.executorLock = &sync.Mutex{}
	}
	r.executorLock.Lock()
	defer r.executorLock.Unlock()
	if r.executorClosing {
		return nil, &pyRuntimeError{msg: "Gateway is shutting down; executor unavailable"}
	}
	if *slot == nil || (*slot).isShutdown() {
		*slot = makePool()
	}
	return *slot, nil
}

// Unbounded turn pool for turn bodies and other blocking agent work
func (r *GatewayRunner) getExecutor() (gatewayExecutor, error) {
	return r.getOrCreatePool(&r.executor, func() gatewayExecutor {
		return newUnboundedThreadExecutor("icebath-gateway", turnExecutorIdleTimeoutS)
	})
}

// Bounded pool for session housekeeping chores
func (r *GatewayRunner) getHousekeepingExecutor() (gatewayExecutor, error) {
	return r.getOrCreatePool(&r.housekeepingExecutor, func() gatewayExecutor {
		return newThreadPoolExecutor(housekeepingMaxWorkers, "icebath-gateway-hk")
	})
}

// Shuts the pool down without waiting, returning its threads to join
func stopPool(executor gatewayExecutor) []executorWorker {
	if executor == nil {
		return nil
	}
	executor.shutdown(false, true)
	return executor.threads()
}

// Stops both pools and joins their workers within the drain budget, returning how many still run
func (r *GatewayRunner) shutdownExecutor(drainTimeout float64) int {
	if r.executorLock == nil {
		return 0
	}
	r.executorLock.Lock()
	r.executorClosing = true
	executor, housekeeping := r.executor, r.housekeepingExecutor
	r.executor, r.housekeepingExecutor = nil, nil
	r.executorLock.Unlock()
	workers := append(stopPool(executor), stopPool(housekeeping)...)
	deadline := pythread.Monotonic() + math.Max(drainTimeout, 0)
	for _, worker := range workers {
		remaining := deadline - pythread.Monotonic()
		if remaining <= 0 {
			break
		}
		worker.join(remaining)
	}
	live := 0
	for _, worker := range workers {
		if worker.isAlive() {
			live++
		}
	}
	return live
}
