package gatewayrun

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Authority work an unserve of a profile stops, managed workers, cron runs and side agents
type unserveStopCounter interface {
	UnserveStopCounts() (managed, crons, sides int)
}

// Runner surface the active work verb reads inside its loop callback
type activeWorkRunner interface {
	describeActiveWork(ctx context.Context) []map[string]any
	activeWorkCount() int
	servedProfileAuthority(profile string) unserveStopCounter
}

// Authority serving the named profile's home, nil without a served home or a registry
func (r *GatewayRunner) servedProfileAuthority(profile string) unserveStopCounter {
	if r.servedProfileHomes == nil || r.sessionAuthorities == nil {
		return nil
	}
	profileHome, ok := r.servedProfileHomes.Get(profile)
	if !ok {
		return nil
	}
	if authority := r.sessionAuthorities.ForHome(profileHome); authority != nil {
		return authority
	}
	return nil
}

// One unit per live async delegation with its goal and elapsed seconds since dispatch
func delegationUnits(now float64) []map[string]any {
	units := []map[string]any{}
	for _, record := range tools.ListAsyncDelegations() {
		if !tools.AsyncDelegationLive(record["status"]) {
			continue
		}
		goal := ""
		if pyrepr.Truthy(record["goal"]) {
			goal = pyrepr.Str(record["goal"])
		}
		if runes := []rune(goal); len(runes) > 80 {
			goal = string(runes[:80])
		}
		unit := map[string]any{"kind": "delegation", "delegation_id": record["delegation_id"], "goal": goal, "pid": os.Getpid()}
		if pyrepr.Truthy(record["dispatched_at"]) {
			dispatched, err := pyint.Float(record["dispatched_at"])
			if err != nil {
				panic(err)
			}
			unit["elapsed_s"] = pyint.Round(now-dispatched, 1)
		}
		units = append(units, unit)
	}
	return units
}

// Units an unserve of the profile stops, its chat turns plus its authority's managed turns, cron runs and side agents
func profileUnits(runner activeWorkRunner, profile string, units []map[string]any) []map[string]any {
	prefix := gateway.SessionKeyNamespace(profile) + ":"
	mine := []map[string]any{}
	for _, u := range units {
		session := ""
		if pyrepr.Truthy(u["session"]) {
			session = pyrepr.Str(u["session"])
		}
		if u["kind"] == "chat" && strings.HasPrefix(session, prefix) {
			mine = append(mine, u)
		}
	}
	managed, crons, sides := 0, 0, 0
	if authority := runner.servedProfileAuthority(profile); authority != nil {
		managed, crons, sides = authority.UnserveStopCounts()
	}
	pid := os.Getpid()
	for range managed {
		mine = append(mine, map[string]any{"kind": "managed", "pid": pid})
	}
	for range crons {
		mine = append(mine, map[string]any{"kind": "cron", "job_id": "?", "pid": pid})
	}
	for range sides {
		mine = append(mine, map[string]any{"kind": "side_agent", "pid": pid})
	}
	return mine
}

// Control socket handler naming the live work a stop or an unserve would interrupt, read on the gateway loop
func activeWorkVerb(ctx context.Context, runner activeWorkRunner, loop gateway.EventLoop) func(params map[string]any) (map[string]any, error) {
	return func(params map[string]any) (map[string]any, error) {
		profile, _ := params["profile"].(string)
		scoped := profile != ""
		var box []map[string]any
		done := make(chan struct{})
		collect := func() {
			defer close(done)
			units := append(append([]map[string]any{}, runner.describeActiveWork(ctx)...), delegationUnits(wallClock())...)
			if scoped {
				units = profileUnits(runner, profile, units)
				box = append(box, map[string]any{"active_agents": len(units), "active_work": units, "profile": profile})
			} else {
				box = append(box, map[string]any{"active_agents": runner.activeWorkCount(), "active_work": units})
			}
		}
		if err := loop.CallSoonThreadsafe(collect); err != nil {
			return nil, err
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			return nil, errors.New("active-work: the gateway loop did not answer")
		}
		if len(box) == 0 {
			return nil, errors.New("active-work: the gateway loop did not answer")
		}
		answer := map[string]any{}
		for k, v := range box[0] {
			answer[k] = v
		}
		answer["pid"] = os.Getpid()
		return answer, nil
	}
}
