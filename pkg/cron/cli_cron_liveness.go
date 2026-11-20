package cron

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Resolved cron scheduler provider name, builtin when resolution yields no name
func (s *Store) activeCronProviderName() string {
	if sched := resolveCronScheduler(s); sched != nil && sched.Name() != "" {
		return sched.Name()
	}
	return "builtin"
}

// Whether a ticker heartbeat age is recent enough to prove a live scheduler
func tickerAgeIsFresh(age float64, ok bool) bool {
	return ok && age <= TickerIntervalSeconds*3+20
}

// Tri state readiness of a scheduler serving this home, known false when the probe failed
func (s *Store) builtinGatewayLiveness(ctx context.Context) (alive, known bool) {
	defer func() {
		if r := recover(); r != nil {
			alive, known = false, false
		}
	}()
	if s.activeCronProviderName() != "builtin" {
		return true, true
	}
	if active, err := gateway.IsGatewayRuntimeLockActive(gateway.GatewayRuntimeLockPath(s.home)); err == nil && active {
		return true, true
	}
	pids, err := gateway.FindGatewayPIDs(ctx, s.home)
	if err != nil {
		return false, false
	}
	if len(pids) > 0 {
		return true, true
	}
	if !tickerAgeIsFresh(s.TickerHeartbeatAge()) {
		return false, true
	}
	if gateway.NamedProfileServedByRunningMultiplexer == nil {
		return false, false
	}
	profile, _ := gateway.ProfileNameForHome(s.home)
	return gateway.NamedProfileServedByRunningMultiplexer(profile) || s.TickerHeartbeatWriterAlive(), true
}
