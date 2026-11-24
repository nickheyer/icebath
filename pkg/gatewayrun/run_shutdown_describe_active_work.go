package gatewayrun

import (
	"context"
	"os"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// One entry per in flight work unit the restart wait holds for, an unreadable source omitted
func (r *GatewayRunner) describeActiveWork(ctx context.Context) []map[string]any {
	now := wallClock()
	pid := os.Getpid()
	units := []map[string]any{}
	for key, state := range r.sessionsMap() {
		state.mu.Lock()
		a, startedTS := state.turn.agent, state.turn.startedTS
		state.mu.Unlock()
		if a == nil {
			continue
		}
		unit := map[string]any{"kind": "chat", "session": key, "pid": pid}
		if startedTS != 0 {
			unit["elapsed_s"] = mathutil.Round(now-startedTS, 1)
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
	units = append(units, cronJobUnits(ctx, pid)...)
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
