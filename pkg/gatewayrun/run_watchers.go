package gatewayrun

import (
	"context"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Seconds between SessionStore prunes of stale routing entries
const sessionStorePruneInterval = 3600.0

// Sleeps in one second steps so a watcher stops quickly once the runner stops
func interruptibleSleep(ctx context.Context, r *GatewayRunner, seconds int) error {
	for range seconds {
		if !r.running.Load() {
			break
		}
		if err := sleepCtx(ctx, 1); err != nil {
			return err
		}
	}
	return nil
}

// Reclaims resources on an interval without ending durable conversations
func (r *GatewayRunner) sessionHousekeepingWatcher(ctx context.Context, interval int) error {
	if err := sleepCtx(ctx, 60); err != nil {
		return err
	}
	for r.running.Load() {
		if err := r.sessionHousekeeping(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			runLog().Debug(fmt.Sprintf("Session housekeeping error: %s", pyrepr.ExceptionMessage(err)))
		}
		if err := interruptibleSleep(ctx, r, interval); err != nil {
			return err
		}
	}
	return nil
}

// Runs one guarded sweep, a raised failure logged at debug under its label
func guardedSweep(label string, sweep func()) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug(fmt.Sprintf("%s: %s", label, pyrepr.Str(rec)))
		}
	}()
	sweep()
}

// Idle and pressure agent cache sweeps plus the hourly SessionStore prune
func (r *GatewayRunner) sessionHousekeeping(ctx context.Context) error {
	guardedSweep("Idle agent sweep failed", func() {
		if evicted := r.sweepIdleCachedAgents(ctx); evicted > 0 {
			runLog().Info(fmt.Sprintf("Agent cache idle sweep: evicted %d agent(s)", evicted))
		}
	})
	guardedSweep("Agent cache pressure sweep failed", func() { r.sweepAgentCacheUnderPressure(ctx) })
	now := float64(time.Now().UnixNano()) / 1e9
	if now-r.lastSessionStorePruneTs > sessionStorePruneInterval {
		maxAge := 0
		if r.config != nil && r.config.SessionStoreMaxAgeDays != nil {
			maxAge = int(r.config.SessionStoreMaxAgeDays.Int64())
		}
		if maxAge > 0 {
			n, err := r.asyncSessionStore.Store().PruneOldEntries(ctx, maxAge)
			if err != nil {
				runLog().Debug(fmt.Sprintf("SessionStore prune failed: %s", pyrepr.ExceptionMessage(err)))
			} else if n > 0 {
				runLog().Info(fmt.Sprintf("SessionStore prune: dropped %d stale entries", n))
			}
		}
		r.lastSessionStorePruneTs = float64(time.Now().UnixNano()) / 1e9
	}
	return nil
}
