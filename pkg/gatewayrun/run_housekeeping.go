package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	orderedutil "github.com/nickheyer/icebath/pkg/utils/ordered"
	syncutil "github.com/nickheyer/icebath/pkg/utils/sync"
)

// Housekeeping interval in seconds, the cadence unit of every chore
const housekeepingIntervalSecs = 60

// Adapters, loop, cron ticker and runner the housekeeping goroutine works with
type housekeepingArgs struct {
	adapters *orderedutil.Map[gateway.Platform, gateway.PlatformAdapter]
	loop     gateway.EventLoop
	cron     cronTicker
	runner   *GatewayRunner
}

// Schedules fn as a loop task from another goroutine, nil with a debug log when the loop refuses it
func scheduleOnLoop(ctx context.Context, loop gateway.EventLoop, name string, fn func(ctx context.Context) (any, error),
	log *slog.Logger, logMessage string) *gatewayFuture {
	if loop == nil {
		log.Debug(logMessage + ": loop is None")
		return nil
	}
	fut := newGatewayFuture()
	if err := loop.CallSoonThreadsafe(func() {
		task := spawnGatewayTask(ctx, name, fn)
		task.addDoneCallback(func(f *gatewayFuture) { fut.settle(f.result, f.err) })
	}); err != nil {
		log.Debug(fmt.Sprintf("%s: %s", logMessage, err))
		return nil
	}
	return fut
}

// Result of a future once it settles within timeout, a TimeoutError when it has not
func futureResultWithin(ctx context.Context, fut *gatewayFuture, timeout time.Duration) (any, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := fut.wait(waitCtx)
	if !fut.isDone() && ctx.Err() == nil {
		return nil, &errorutil.Error{Kind: "TimeoutError"}
	}
	return result, err
}

// Rebuilds the channel directory on the gateway loop, waiting up to thirty seconds so failures log
func housekeepingChannelDirectory(ctx context.Context, adapters *orderedutil.Map[gateway.Platform, gateway.PlatformAdapter], loop gateway.EventLoop) error {
	if loop == nil {
		return nil
	}
	fut := scheduleOnLoop(ctx, loop, "channel-directory-refresh", func(c context.Context) (any, error) {
		return gateway.BuildChannelDirectory(c, adapters)
	}, pkgLog(), "Channel directory refresh scheduling error")
	if fut == nil {
		return nil
	}
	_, err := futureResultWithin(ctx, fut, 30*time.Second)
	return err
}

// Deletes expired pending pastes and logs how many went
func housekeepingPasteSweep(ctx context.Context) error {
	deleted, remaining, err := SweepExpiredPastes(ctx, nil)
	if err != nil {
		return err
	}
	if deleted > 0 {
		pkgLog().Info(fmt.Sprintf("Paste sweep: deleted %d expired paste(s), %d pending", deleted, remaining))
	}
	return nil
}

// Curator poll, gated inside by its configured interval
func housekeepingCurator(ctx context.Context) error {
	idle := math.Inf(1)
	memory.MaybeRunCurator(ctx, &idle, func(msg string) { pkgLog().Info("curator: " + msg) })
	return nil
}

// Plugin update check, due gated by plugins.auto_update_check_hours
func housekeepingPluginUpdateCheck(ctx context.Context) error {
	_, _, err := plugins.MaybeRunGatewayCheck(ctx, plugins.ScheduledCheckOptions{Log: pkgLog()})
	return err
}

// Retries a deferred FTS rebuild on every live shared session database this process holds
func housekeepingDeferredFTSRetry(ctx context.Context) error {
	return state.BorrowLiveSharedSessionDBs(func(dbs []*state.DB) error {
		for _, db := range dbs {
			done, err := db.RetryDeferredFTSRecovery(ctx)
			if err != nil {
				return err
			}
			if done {
				pkgLog().Info(fmt.Sprintf("Deferred state.db FTS rebuild completed in-process for %s; full-text search restored.", db.Path()))
			}
		}
		return nil
	})
}

// Allocator release every tick and one full collection per quiet period
func housekeepingMemoryTrim(ctx context.Context) error {
	env := &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}
	memory.TrimMemoryWhenIdle(ctx, env, "messaging gateway housekeeping", agent.ReviewQueueQuietClock{})
	return nil
}

// Checkpoint store retention and size cap, gated by checkpoints.auto_prune and its marker
func housekeepingCheckpointPrune(ctx context.Context) error {
	tools.AutoPruneCheckpointsFromConfig(&tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)})
	return nil
}

// Chore keeping live MCP servers in step with mcp_servers on disk, every tick after the first per profile
func mcpConfigReconciler(ctx context.Context, r *GatewayRunner) func() error {
	primed := map[string]bool{}
	reconcileCurrent := func(ctx context.Context, label string) error {
		if !primed[label] {
			primed[label] = true
			return nil
		}
		result, err := mcp.Reconcile(mcp.SuppressInteractiveOAuth(ctx), nil)
		if err != nil {
			return err
		}
		if len(result.Removed) > 0 || len(result.Added) > 0 {
			pkgLog().Info(fmt.Sprintf("MCP servers reconciled with config (%s): removed=%s added=%s",
				label, errchain.Show(result.Removed), errchain.Show(result.Added)))
		}
		return nil
	}
	return func() error { return forEachServedProfile(ctx, r, reconcileCurrent) }
}

// One housekeeping chore with its cadence in ticks
type housekeepingTask struct {
	every int
	label string
	fn    func() error
}

// Gateway only periodic chores on their own goroutine until stop is set, the queue file watched between ticks
func startGatewayHousekeeping(ctx context.Context, stop *syncutil.Event, a housekeepingArgs) {
	interval := housekeepingIntervalSecs
	adapters, loop, r := a.adapters, a.loop, a.runner
	hasSources := adapters != nil || r != nil
	drains := hasSources && cronDeliveryDrain != nil
	drain := func() error { return cronDeliveryDrain(ctx, adapters, loop, r) }
	chores := []housekeepingTask{{1, "Runtime heartbeat", func() error {
		writeRuntimeStatusQuiet(ctx, gateway.RuntimeStatusUpdate{})
		return nil
	}}}
	if drains {
		chores = append(chores, housekeepingTask{1, deliveryQueueDrainLabel, drain})
	}
	chores = append(chores,
		housekeepingTask{5, "Channel directory refresh", func() error {
			if adapters == nil || adapters.Len() == 0 {
				return nil
			}
			return housekeepingChannelDirectory(ctx, adapters, loop)
		}},
		housekeepingTask{60, "Media cache cleanup", func() error { housekeepingMediaCaches(ctx); return nil }},
		housekeepingTask{60, "Paste sweep", func() error { return housekeepingPasteSweep(ctx) }},
		housekeepingTask{1, "Auto-prune low-disk prompt", func() error { return maybePostAutoPrunePrompt(ctx, r, loop) }})
	if a.cron != nil {
		chores = append(chores, a.cron.chores(ctx, adapters, loop)...)
	}
	var cfg *gateway.GatewayConfig
	if r != nil {
		cfg = r.config
	}
	launch := launchSessionsDir(ctx, cfg)
	chores = append(chores,
		housekeepingTask{60, "Curator tick", profileScopedChore(ctx, r, housekeepingCurator)},
		housekeepingTask{60, "state.db maintenance tick", profileScopedChore(ctx, r, func(c context.Context) error {
			return housekeepingStateDBMaintenance(c, launch)
		})},
		housekeepingTask{1, "Plugin update check", profileScopedChore(ctx, r, housekeepingPluginUpdateCheck)},
		housekeepingTask{1, "Deferred FTS retry tick", func() error { return housekeepingDeferredFTSRetry(ctx) }},
		housekeepingTask{1, "gateway housekeeping memory trim", func() error { return housekeepingMemoryTrim(ctx) }},
		housekeepingTask{1, "MCP config reconcile", mcpConfigReconciler(ctx, r)},
		housekeepingTask{1, "Checkpoint prune tick", func() error { return housekeepingCheckpointPrune(ctx) }})
	var watch *DeliveryQueueWatch
	if drains {
		servedHomes := func() []*string {
			if r == nil {
				return []*string{nil}
			}
			var homes []*string
			for _, scope := range handoffWatchScopes(ctx, r) {
				if scope.home == "" {
					homes = append(homes, nil)
					continue
				}
				h := scope.home
				homes = append(homes, &h)
			}
			return homes
		}
		watch = NewDeliveryQueueWatch(ctx, servedHomes, func() { housekeepingChore(deliveryQueueDrainLabel, drain) })
	}
	pkgLog().Info(fmt.Sprintf("Gateway housekeeping started (interval=%ds)", interval))
	tick := 0
	chore := func(label string, fn func()) { housekeepingChore(label, func() error { fn(); return nil }) }
	for !stop.IsSet() {
		tick++
		for _, c := range chores {
			if tick%c.every == 0 {
				housekeepingChore(c.label, c.fn)
			}
		}
		waitForNextTick(ctx, stop, time.Duration(interval)*time.Second, watch, chore)
	}
	pkgLog().Info("Gateway housekeeping stopped")
}
