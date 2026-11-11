package gatewayrun

import (
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
)

// Every unit of agent work the gateway exposes and drains as one total
func (r *GatewayRunner) activeWorkCount() int {
	return r.runningAgentCount() + gateway.ManagedTurnCount(r) + r.activeCronJobCount() +
		r.activeAPIRunCount() + r.activeDeferredAgentWorkerCount()
}

// Cron jobs executing on the scheduler pool outside the running agent map
func runningCronJobCount() int {
	return len(cron.GetRunningJobIDs())
}

// Cron jobs currently executing, zero when the count cannot be read
func (r *GatewayRunner) activeCronJobCount() (n int) {
	defer func() {
		if rec := recover(); rec != nil {
			n = 0
		}
	}()
	return runningCronJobCount()
}

// API server adapter exposing its own live agent work count
type activeAgentWorkCounter interface {
	ActiveAgentWorkCount() int
}

// Live agent work of the primary API server adapter clamped at zero, zero without the hook
func (r *GatewayRunner) apiServerActiveAgentWorkCount() int {
	adapter, _ := r.adapters.Get(gateway.PlatformAPIServer)
	counter, ok := adapter.(activeAgentWorkCounter)
	if !ok {
		return 0
	}
	return max(0, counter.ActiveAgentWorkCount())
}

// API server work outside the running agent map, zero when the hook fails
func (r *GatewayRunner) activeAPIRunCount() (n int) {
	defer func() {
		if rec := recover(); rec != nil {
			n = 0
		}
	}()
	return r.apiServerActiveAgentWorkCount()
}

// Executor workers that outlived their gateway turn and have not finished
func (r *GatewayRunner) activeDeferredAgentWorkerCount() int {
	r.deferredAgentWorkersMu.Lock()
	defer r.deferredAgentWorkersMu.Unlock()
	n := 0
	for future := range r.deferredAgentWorkers {
		if !future.isDone() {
			n++
		}
	}
	return n
}

// Exposes an executor worker to drain and interrupt until it really exits
func (r *GatewayRunner) trackDeferredAgentWorker(future *gatewayFuture, a *agent.AIAgent) {
	r.deferredAgentWorkersMu.Lock()
	if r.deferredAgentWorkers == nil {
		r.deferredAgentWorkers = map[*gatewayFuture]*agent.AIAgent{}
	}
	r.deferredAgentWorkers[future] = a
	r.deferredAgentWorkersMu.Unlock()
	future.addDoneCallback(func(done *gatewayFuture) {
		r.deferredAgentWorkersMu.Lock()
		delete(r.deferredAgentWorkers, done)
		r.deferredAgentWorkersMu.Unlock()
	})
}
