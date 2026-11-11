package gatewayrun

import (
	"context"
	"os"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
)

// One entry per in flight work unit the restart wait holds for, an unreadable source omitted
func (r *GatewayRunner) describeActiveWork(ctx context.Context) []map[string]any {
	now := wallClock()
	pid := os.Getpid()
	units := []map[string]any{}
	for key, state := range r.sessionsMap() {
		a := state.turn.agent
		if a == nil {
			continue
		}
		unit := map[string]any{"kind": "chat", "session": key, "pid": pid}
		if state.turn.startedTS != 0 {
			unit["elapsed_s"] = pyint.Round(now-state.turn.startedTS, 1)
		}
		if a != agentPendingSentinel {
			unit["model"] = a.Model()
			func() {
				defer func() { _ = recover() }()
				summary := a.GetActivitySummary()
				unit["current_tool"] = summary["current_tool"]
				unit["idle_s"] = summary["seconds_since_activity"]
			}()
		}
		units = append(units, unit)
	}
	func() {
		defer func() { _ = recover() }()
		wedged := cron.GetWedgedJobIDs(ctx)
		for _, job := range cron.GetRunningJobDetails() {
			jobPID := pid
			if job.WorkerPID != nil && *job.WorkerPID != 0 {
				jobPID = *job.WorkerPID
			}
			var elapsed any
			if job.ElapsedS != nil {
				elapsed = *job.ElapsedS
			}
			units = append(units, map[string]any{"kind": "cron", "job_id": job.JobID, "elapsed_s": elapsed,
				"pid": jobPID, "external": job.WorkerPID != nil && *job.WorkerPID != 0,
				"wedged": wedged[job.JobID], "restart_safe": job.RestartSafe})
		}
	}()
	counts := []struct {
		kind  string
		count int
	}{{"managed", gateway.ManagedTurnCount(r)}, {"api", r.activeAPIRunCount()}, {"deferred", r.activeDeferredAgentWorkerCount()}}
	for _, c := range counts {
		for range c.count {
			units = append(units, map[string]any{"kind": c.kind, "pid": pid})
		}
	}
	return units
}
