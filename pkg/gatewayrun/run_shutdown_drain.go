package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Chat, cron, API and deferred work the drain waits on, a managed worker turn counting as chat
type drainWorkCounts struct {
	agents, cron, api, deferred int
}

// Whether any unit of the counts is still live
func (c drainWorkCounts) any() bool {
	return c.agents != 0 || c.cron != 0 || c.api != 0 || c.deferred != 0
}

// Current work counts the drain waits on
func (r *GatewayRunner) drainWorkCounts() drainWorkCounts {
	return drainWorkCounts{agents: r.runningAgentCount() + gateway.ManagedTurnCount(r), cron: r.activeCronJobCount(),
		api: r.activeAPIRunCount(), deferred: r.activeDeferredAgentWorkerCount()}
}

// Waits for chat work up to timeout and cron or API work up to cronTimeout, returning the snapshot and whether work remained
func (r *GatewayRunner) drainActiveAgents(ctx context.Context, timeout float64, cronTimeout *float64) (map[string]*agent.AIAgent, bool, error) {
	snapshot := r.snapshotRunningAgents()
	lastCounts := r.drainWorkCounts()
	lastStatusAt := 0.0
	maybeUpdateStatus := func(force bool) {
		now := pythread.Monotonic()
		counts := r.drainWorkCounts()
		if force || counts != lastCounts || now-lastStatusAt >= 1.0 {
			r.updateRuntimeStatus(ctx, optStr("draining"), nil)
			lastCounts, lastStatusAt = counts, now
		}
	}
	maybeUpdateStatus(true)
	if r.runningAgentCount() == 0 && !lastCounts.any() {
		return snapshot, false, nil
	}
	started := pythread.Monotonic()
	deadline := started + timeout
	cronDeadline := started + timeout
	if cronTimeout != nil {
		cronDeadline = started + *cronTimeout
	}
	stillDraining := func() bool {
		now := pythread.Monotonic()
		c := r.drainWorkCounts()
		return ((c.agents != 0 || c.deferred != 0) && now < deadline) || ((c.cron != 0 || c.api != 0) && now < cronDeadline)
	}
	for stillDraining() {
		maybeUpdateStatus(false)
		if err := sleepCtx(ctx, 0.1); err != nil {
			return snapshot, false, err
		}
	}
	timedOut := r.drainWorkCounts().any()
	maybeUpdateStatus(true)
	return snapshot, timedOut, nil
}

// Hard interrupts every running agent, then API runs, deferred workers, managed turns and side agents
func (r *GatewayRunner) interruptRunningAgents(reason string) {
	for _, item := range r.runningAgentItems() {
		if item.agent == agentPendingSentinel {
			continue
		}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug(fmt.Sprintf("Failed interrupting agent during shutdown: %v", rec))
				}
			}()
			agent.RequestHardInterrupt(item.agent, reason, interruptToolReasonGatewayShutdown)
			runLog().Debug(fmt.Sprintf("Interrupted running agent for session %s during shutdown", item.sessionKey))
		}()
	}
	counts := []struct {
		count int
		what  string
	}{
		{r.interruptAPIServerRuns(reason), "api_server run(s)"},
		{r.interruptDeferredAgentWorkers(reason), "deferred agent worker(s)"},
		{gateway.StopManagedTurns(r, &reason), "managed-worker turn(s)"},
		{gateway.InterruptSideAgents(r, reason), "side agent(s)"},
	}
	for _, c := range counts {
		if c.count != 0 {
			runLog().Debug(fmt.Sprintf("Interrupted %d %s during shutdown", c.count, c.what))
		}
	}
}

// Restart interrupt reason while a restart was requested, the shutdown reason otherwise
func (r *GatewayRunner) shutdownInterruptReason() string {
	if r.restartRequested {
		return interruptReasonGatewayRestart
	}
	return interruptReasonGatewayShutdown
}

// Marks every running non pending session resume pending and returns the keys it marked
func (r *GatewayRunner) markRunningSessionsResumePending(ctx context.Context, logPrefix string) []string {
	reason := "shutdown_timeout"
	if r.restartRequested {
		reason = "restart_timeout"
	}
	marked := []string{}
	for _, item := range r.runningAgentItems() {
		if item.agent == agentPendingSentinel {
			continue
		}
		if _, err := r.AsyncSessionStore().Store().MarkResumePending(ctx, item.sessionKey, reason); err != nil {
			logSuppressed(slog.LevelDebug, err, "%s failed for %s: %v", logPrefix, item.sessionKey)
			continue
		}
		marked = append(marked, item.sessionKey)
	}
	return marked
}

// Session keys of the running agent snapshot in a stable order
func sortedAgentKeys(agents map[string]*agent.AIAgent) []string {
	return slices.Sorted(maps.Keys(agents))
}
