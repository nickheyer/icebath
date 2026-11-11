package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Housekeeping interval in seconds, the cadence unit of every chore
const housekeepingIntervalSecs = 60

// Adapters, loop, cron provider, runner and cron ticker the housekeeping goroutine works with
type housekeepingArgs struct {
	adapters     *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]
	loop         gateway.EventLoop
	cronProvider cron.CronScheduler
	runner       *GatewayRunner
	cronThread   *cron.SupervisedTickerThread
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
		return nil, &gateway.PyError{Kind: "TimeoutError"}
	}
	return result, err
}

// Rebuilds the channel directory on the gateway loop, waiting up to thirty seconds so failures log
func housekeepingChannelDirectory(ctx context.Context, adapters *pydict.Dict[gateway.Platform, gateway.PlatformAdapter], loop gateway.EventLoop) error {
	if loop == nil {
		return nil
	}
	fut := scheduleOnLoop(ctx, loop, "channel-directory-refresh", func(c context.Context) (any, error) {
		return gateway.BuildChannelDirectory(c, adapters)
	}, runLog(), "Channel directory refresh scheduling error")
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
		runLog().Info(fmt.Sprintf("Paste sweep: deleted %d expired paste(s), %d pending", deleted, remaining))
	}
	return nil
}

// Fires jobs an external cron provider never fired once their misfire grace passed
func housekeepingMisfireCatchUp(ctx context.Context, p cron.CronScheduler, adapters, loop any) error {
	caughtUp, err := cron.FireOverdueJobs(ctx, p, adapters, loop)
	if err != nil {
		return err
	}
	if caughtUp > 0 {
		runLog().Info(fmt.Sprintf("Misfire catch-up: fired %d overdue job(s)", caughtUp))
	}
	return nil
}

// Curator poll, gated inside by its configured interval
func housekeepingCurator(ctx context.Context) error {
	idle := math.Inf(1)
	memory.MaybeRunCurator(ctx, &idle, func(msg string) { runLog().Info("curator: " + msg) })
	return nil
}

// Plugin update check, due gated by plugins.auto_update_check_hours
func housekeepingPluginUpdateCheck(ctx context.Context) error {
	_, _, err := plugins.MaybeRunGatewayCheck(ctx, plugins.ScheduledCheckOptions{Log: runLog()})
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
				runLog().Info(fmt.Sprintf("Deferred state.db FTS rebuild completed in-process for %s; full-text search restored.", db.Path()))
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

// Drains each profile's worker queue through its live adapters, a satellite through the primary's routed ones
func drainRestartSafeCronDeliveries(ctx context.Context, adapters *pydict.Dict[gateway.Platform, gateway.PlatformAdapter],
	loop gateway.EventLoop, r *GatewayRunner) error {
	var loopArg any
	if loop != nil {
		loopArg = loop
	}
	if r == nil {
		if adapters != nil {
			_, err := cron.DrainDeliveryQueue(ctx, adapters, loopArg)
			return err
		}
		return nil
	}
	for _, scope := range handoffWatchScopes(ctx, r) {
		var profileAdapters any
		if scope.name == nil {
			if adapters != nil {
				profileAdapters = adapters
			}
		} else if r.profileAdapters != nil {
			if m, ok := r.profileAdapters.Get(*scope.name); ok && m != nil {
				profileAdapters = m
			}
		}
		if profileAdapters == nil {
			continue
		}
		h := scope.home
		if h == "" {
			h = home.Dir(ctx)
		}
		scoped := gateway.ProfileRuntimeScope(ctx, h, true)
		var owned any
		if scope.name != nil {
			if platforms, ok := r.profileBotPlatforms[*scope.name]; ok {
				owned = platforms
			}
		}
		if scope.name != nil && adapters != nil && adapters.Len() > 0 && !cron.HasCredentialPlatform(profileAdapters, owned) {
			if routes := cron.PrimaryProfileRoutesForCurrentHome(scoped); len(routes) > 0 {
				profileAdapters = cron.NewSharedRouteAdapters(adapters, routes)
			}
		}
		if _, err := cron.DrainDeliveryQueue(scoped, profileAdapters, loopArg); err != nil {
			return err
		}
	}
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
			runLog().Info(fmt.Sprintf("MCP servers reconciled with config (%s): removed=%s added=%s",
				label, pyrepr.Repr(result.Removed), pyrepr.Repr(result.Added)))
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
func startGatewayHousekeeping(ctx context.Context, stop *pythread.Event, a housekeepingArgs) {
	interval := housekeepingIntervalSecs
	adapters, loop, r := a.adapters, a.loop, a.runner
	hasSources := adapters != nil || r != nil
	drain := func() error { return drainRestartSafeCronDeliveries(ctx, adapters, loop, r) }
	chores := []housekeepingTask{{1, "Runtime heartbeat", func() error {
		writeRuntimeStatusQuiet(ctx, gateway.RuntimeStatusUpdate{})
		return nil
	}}}
	if hasSources {
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
	if a.cronProvider != nil {
		var adaptersArg, loopArg any
		if adapters != nil {
			adaptersArg = adapters
		}
		if loop != nil {
			loopArg = loop
		}
		chores = append(chores, housekeepingTask{5, "Misfire catch-up sweep", func() error {
			return housekeepingMisfireCatchUp(ctx, a.cronProvider, adaptersArg, loopArg)
		}})
	}
	if a.cronThread != nil {
		chores = append(chores, housekeepingTask{1, "Cron ticker supervisor", func() error { a.cronThread.RestartIfDead(); return nil }})
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
	if hasSources {
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
	runLog().Info(fmt.Sprintf("Gateway housekeeping started (interval=%ds)", interval))
	tick := 0
	chore := func(label string, fn func()) { housekeepingChore(label, func() error { fn(); return nil }) }
	for !stop.IsSet() {
		tick++
		for _, c := range chores {
			if tick%c.every == 0 {
				housekeepingChore(c.label, c.fn)
			}
		}
		waitForNextTick(ctx, stop, float64(interval), watch, chore)
	}
	runLog().Info("Gateway housekeeping stopped")
}
