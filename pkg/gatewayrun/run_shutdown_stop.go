package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Runs one teardown step, a failure debug logged as label and the error
func quietStep[T any](label string, fn func() (T, error)) (out T) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug(fmt.Sprintf("%s: %v", label, rec))
			var zero T
			out = zero
		}
	}()
	value, err := fn()
	if err != nil {
		runLog().Debug(fmt.Sprintf("%s: %v", label, err))
		var zero T
		return zero
	}
	return value
}

// Kills tool subprocesses, terminal environments and browsers, returning the cron job ids marked interrupted
func stopKillToolSubprocesses(ctx context.Context, phase string) []string {
	env := &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}
	step := func(label string, fn func() error) {
		quietStep(label+" ("+phase+") error", func() (struct{}, error) { return struct{}{}, fn() })
	}
	countStep := func(format string, fn func() int) {
		if n := fn(); n != 0 {
			runLog().Info(fmt.Sprintf(format, phase, n))
		}
	}
	step("process_registry.kill_all", func() error {
		countStep("Shutdown (%s): killed %d tool subprocess(es)", func() int {
			return tools.DefaultProcessRegistry().KillAll(tools.WithEnvHome(ctx, env), env, "", nil, "gateway_shutdown", false)
		})
		return nil
	})
	marked := quietStep("mark_running_jobs_interrupted ("+phase+") error", func() ([]string, error) {
		interrupted := cron.MarkRunningJobsInterrupted(ctx,
			fmt.Sprintf("Gateway shutdown (%s) killed the job's tool subprocess before the run finished.", phase))
		if len(interrupted) > 0 {
			runLog().Warn(fmt.Sprintf("Shutdown (%s): marked %d in-flight cron job(s) interrupted: %s",
				phase, len(interrupted), strings.Join(interrupted, ", ")))
		}
		return interrupted, nil
	})
	step("async interrupt_all", func() error {
		countStep("Shutdown (%s): interrupted %d background delegation(s)", func() int {
			return tools.InterruptAllAsyncDelegations("gateway shutdown (" + phase + ")")
		})
		return nil
	})
	step("cleanup_all_environments", func() error { tools.CleanupAllEnvironments(env); return nil })
	step("cleanup_all_browsers", func() error { browser.CleanupAllBrowsers(ctx, env); return nil })
	if marked == nil {
		return []string{}
	}
	return marked
}

// Runs the tool subprocess sweep on its own goroutine so blocking kills never hold the caller
func stopKillToolSubprocessesOffLoop(ctx context.Context, phase string) ([]string, error) {
	result := make(chan []string, 1)
	go func() { result <- stopKillToolSubprocesses(context.WithoutCancel(ctx), phase) }()
	select {
	case marked := <-result:
		return marked, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Flags teardown, stops the room worker and watchdog, and notifies chats while adapters are connected
func (r *GatewayRunner) stopBeginTeardown(ctx context.Context, sc *stopContext) error {
	suffix := ""
	if r.restartRequested {
		suffix = " for restart"
	}
	runLog().Info(fmt.Sprintf("Stopping gateway%s...", suffix))
	started := pythread.Monotonic()
	sc.startedAt = &started
	r.running = false
	r.clearPluginMessageInjector()
	r.draining = true
	r.drainGatewayRuntime(ctx)
	r.markAPIRunsShutdownRequested()
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Warn("Group Chat worker could not stop cleanly; the next gateway start will recover durable work", "exc_info", rec)
			}
		}()
		if !r.stopHostedRoomWorker(ctx, 5.0) {
			runLog().Warn("Group Chat worker is still settling durable work; the next gateway start will recover it")
		}
	}()
	r.stopSystemdWatchdog(ctx)
	if err := r.cancelSecondaryProfileReconnectTasks(ctx); err != nil {
		return err
	}
	r.notifyActiveSessionsOfShutdown(ctx)
	runLog().Info(fmt.Sprintf("Shutdown phase: notify_active_sessions done at +%.2fs", sc.elapsed()))
	return nil
}

// Pre marks resume pending, then drains agent, cron and API work into the stop context
func (r *GatewayRunner) stopDrainActiveWork(ctx context.Context, timeout float64, sc *stopContext) error {
	preDrainKeys := r.markRunningSessionsResumePending(ctx, "pre-drain mark_resume_pending")
	cronAtStart := r.activeCronJobCount()
	apiAtStart := r.activeAPIRunCount()
	deferredAtStart := sc.deferredCount()
	cronDrainCfg := r.cronDrainTimeout
	cronLeash := gateway.EffectiveStopWatchdogDelay(r.stopTiming(), gateway.ResolveShutdownWatchdogDelay(timeout, nil))
	cronTimeout := gateway.ResolveCronDrainBudget(timeout, cronDrainCfg, cronLeash, sc.elapsed(), nil)
	if (cronAtStart != 0 || apiAtStart != 0) && cronTimeout > timeout {
		runLog().Info(fmt.Sprintf("Shutdown drain: %d in-flight cron job(s), %d api_server run(s) — waiting up to "+
			"%.0fs for them (cron_drain_timeout=%.0fs, restart_drain_timeout=%.0fs)",
			cronAtStart, apiAtStart, cronTimeout, cronDrainCfg, timeout))
	}
	drainStartedAt := pythread.Monotonic()
	active, timedOut, err := r.drainActiveAgents(ctx, timeout, &cronTimeout)
	if err != nil {
		return err
	}
	sc.activeAgents, sc.timedOut = active, timedOut
	sc.drainElapsed = pythread.Monotonic() - drainStartedAt
	runLog().Info(fmt.Sprintf("Shutdown phase: drain done at +%.2fs (drain took %.2fs, timed_out=%s, active_at_start=%d, "+
		"active_now=%d, cron_at_start=%d, cron_now=%d, api_at_start=%d, api_now=%d, "+
		"deferred_at_start=%d, deferred_now=%d)", sc.elapsed(), sc.drainElapsed, pyBool(sc.timedOut), len(sc.activeAgents),
		r.runningAgentCount(), cronAtStart, r.activeCronJobCount(), apiAtStart, r.activeAPIRunCount(),
		deferredAtStart, sc.deferredCount()))
	if sc.timedOut {
		return nil
	}
	running := r.snapshotRunningAgentKeys()
	for _, key := range preDrainKeys {
		if running[key] {
			continue
		}
		if _, err := r.AsyncSessionStore().Store().ClearResumePending(ctx, key); err != nil {
			runLog().Debug(fmt.Sprintf("clear_resume_pending after drain failed for %s: %v", key, err))
		}
	}
	return nil
}

// Session keys holding a running turn slot, pending sentinels included
func (r *GatewayRunner) snapshotRunningAgentKeys() map[string]bool {
	keys := map[string]bool{}
	for _, item := range r.runningAgentItems() {
		keys[item.sessionKey] = true
	}
	return keys
}

// Python str of a bool
func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

// After a timed out drain marks resume pending, interrupts, waits the grace, kills tool processes and notifies cron owners
func (r *GatewayRunner) stopInterruptRemainingWork(ctx context.Context, sc *stopContext) error {
	runLog().Warn(fmt.Sprintf("Gateway drain timed out after %.1fs with %d active agent(s), "+
		"%d in-flight cron job(s), %d api_server run(s), and %d deferred agent worker(s); "+
		"interrupting remaining work.", sc.drainElapsed, r.runningAgentCount(),
		r.activeCronJobCount(), r.activeAPIRunCount(), sc.deferredCount()))
	r.markRunningSessionsResumePending(ctx, "mark_resume_pending")
	reason := r.shutdownInterruptReason()
	r.interruptRunningAgents(reason)
	grace := r.postInterruptGraceTimeout()
	deadline := pythread.Monotonic() + grace
	runLog().Info(fmt.Sprintf("Shutdown phase: allowing %.1fs for interrupted agents to unwind", grace))
	workLive := func() bool {
		return r.runningAgentCount() != 0 || gateway.ManagedTurnCount(r) != 0 || r.activeAPIRunCount() != 0 || sc.deferredCount() != 0
	}
	for workLive() && pythread.Monotonic() < deadline {
		r.updateRuntimeStatus(ctx, optStr("draining"), nil)
		if err := sleepCtx(ctx, 0.1); err != nil {
			return err
		}
	}
	if workLive() {
		r.interruptRunningAgents(reason)
		runLog().Debug("Re-signaled interrupt for work still live at settle-window exit")
	}
	interrupted, err := stopKillToolSubprocessesOffLoop(ctx, "post-interrupt")
	if err != nil {
		return err
	}
	runLog().Info(fmt.Sprintf("Shutdown phase: post-interrupt tool kill done at +%.2fs", sc.elapsed()))
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug(fmt.Sprintf("Cron interrupt notification failed: %v", rec))
			}
		}()
		r.notifyInterruptedCronJobs(ctx, interrupted)
	}()
	runLog().Info(fmt.Sprintf("Shutdown phase: cron interrupt notices done at +%.2fs", sc.elapsed()))
	return nil
}

// Launches the detached restart, finalizes agents and the idle cache, then tears every adapter down
func (r *GatewayRunner) stopFinalizeAgentsAndAdapters(ctx context.Context, sc *stopContext) {
	if r.restartRequested && r.restartDetached {
		if err := r.launchDetachedRestartCommand(ctx); err != nil {
			logSuppressed(slog.LevelError, err, "Failed to launch detached gateway restart: %v")
		}
	}
	r.finalizeShutdownAgents(ctx, sc.activeAgents)
	if r.agentCacheLock != nil && r.agentCache != nil {
		r.agentCacheLock.Lock()
		type idle struct {
			key   string
			entry agentCacheEntry
		}
		var idleAgents []idle
		for key, entry := range r.agentCache.All() {
			idleAgents = append(idleAgents, idle{key, entry})
		}
		r.agentCache.Clear()
		r.agentCacheLock.Unlock()
		for _, item := range idleAgents {
			r.cleanupAgentResourcesOffLoop(ctx, item.entry.agent, "shutdown idle-cache", item.key)
		}
	}
	r.cancelProcessCompletionBatchTasks(ctx)
	for _, platform := range r.adapters.Keys() {
		if adapter, ok := r.adapters.Get(platform); ok {
			r.boundedAdapterTeardown(ctx, adapter, platform, "")
		}
	}
	for _, name := range r.profileAdapters.Keys() {
		amap, _ := r.profileAdapters.Get(name)
		for _, platform := range amap.Keys() {
			if adapter, ok := amap.Get(platform); ok {
				r.boundedAdapterTeardown(ctx, adapter, platform, name)
			}
		}
		if amap != nil {
			amap.Clear()
		}
	}
	if r.profileAdapters != nil {
		r.profileAdapters.Clear()
	}
	runLog().Info(fmt.Sprintf("Shutdown phase: all adapters disconnected at +%.2fs", sc.elapsed()))
}

// Cancels background tasks, flushes pending messages, clears per session state and runs the final tool kill
func (r *GatewayRunner) stopReleaseRuntimeState(ctx context.Context, sc *stopContext) error {
	r.taskSetsMu.Lock()
	var tasks []*gatewayTask
	for task := range r.backgroundTasks {
		tasks = append(tasks, task)
	}
	r.taskSetsMu.Unlock()
	for _, task := range tasks {
		if task == r.stopTask || task == r.restartTask {
			continue
		}
		task.Cancel()
	}
	r.taskSetsMu.Lock()
	clear(r.backgroundTasks)
	r.taskSetsMu.Unlock()
	sc.apiLive = r.activeAPIRunCount()
	if r.adapters != nil {
		r.adapters.Clear()
	}
	for key := range r.snapshotRunningAgentKeys() {
		r.releaseRunningAgentState(ctx, key, nil)
	}
	func() {
		defer func() { _ = recover() }()
		pending := map[string]any{}
		for key, text := range r.pendingMessages().Items() {
			if text != nil {
				pending[key] = *text
			} else {
				pending[key] = nil
			}
		}
		_, _ = gateway.FlushPendingValuesToFile(ctx, pending, "shutdown")
	}()
	func() {
		defer func() { _ = recover() }()
		overflow := map[string][]any{}
		for key, events := range r.queuedEvents().Items() {
			if len(events) == 0 {
				continue
			}
			items := make([]any, len(events))
			for i, e := range events {
				items[i] = e
			}
			overflow[key] = items
		}
		_, _ = gateway.FlushOverflowToFile(ctx, overflow, "shutdown")
	}()
	r.runningAgents().Clear()
	r.runningAgentsTS().Clear()
	r.pendingMessages().Clear()
	r.pendingApprovals().Clear()
	r.activeSessionLeases().Clear()
	r.busyAckTS().Clear()
	r.shutdownEvent.Set()
	if _, err := stopKillToolSubprocessesOffLoop(ctx, "final-cleanup"); err != nil {
		return err
	}
	runLog().Info(fmt.Sprintf("Shutdown phase: final-cleanup tool kill done at +%.2fs", sc.elapsed()))
	quietStep("shutdown_cached_clients error", func() (struct{}, error) {
		provider.ShutdownCachedClients(ctx)
		return struct{}{}, nil
	})
	return nil
}

// Quiesces the executor, then closes the session databases only when no worker can still write
func (r *GatewayRunner) stopQuiesceAndCloseSessionDBs(ctx context.Context, timeout float64, sc *stopContext) {
	budget := max(0.0, min(executorQuiesceTimeout, gateway.ResolveShutdownWatchdogDelay(timeout, nil)-sc.elapsed()-1.0))
	if live := r.shutdownExecutor(budget); live != 0 {
		runLog().Warn(fmt.Sprintf("Shutdown phase: %d executor worker(s) still running after a %.2fs quiesce — skipping the "+
			"SessionDB close/checkpoint to avoid racing a live write (#101093); handles are left "+
			"open for SQLite to recover on next open", live, budget))
		return
	}
	runLog().Info(fmt.Sprintf("Shutdown phase: executor quiesced at +%.2fs", sc.elapsed()))
	cronLive := r.activeCronJobCount()
	apiLive := sc.apiLive
	apiWorkerLive := r.activeAPIWorkerCount()
	deferredLive := sc.deferredCount() + gateway.ManagedTurnCount(r)
	if cronLive != 0 || apiLive != 0 || apiWorkerLive != 0 || deferredLive != 0 {
		runLog().Warn(fmt.Sprintf("Shutdown phase: %d cron job(s) / %d API-server run(s) / %d API-server worker(s) / "+
			"%d deferred/authority worker(s) still running after the executor quiesce — skipping the SessionDB "+
			"close/checkpoint, leaving state.db open for the live writer (#102198, #116535)",
			cronLive, apiLive, apiWorkerLive, deferredLive))
		return
	}
	var dbs []interface{ Close() error }
	if db := r.sessionDB(ctx); db != nil && db.DB() != nil {
		dbs = append(dbs, db.DB())
	}
	if r.sessionStore != nil {
		if storeDB, err := r.sessionStore.DB(ctx); err == nil {
			if closer, ok := storeDB.(interface{ Close() error }); ok && closer != nil {
				dbs = append(dbs, closer)
			}
		}
	}
	for _, db := range dbs {
		quietStep("SessionDB close error", func() (struct{}, error) { return struct{}{}, db.Close() })
	}
	if r.sessionStore != nil {
		quietStep("SessionDB handle sweep error", func() (struct{}, error) {
			r.sessionStore.CloseAllDBHandles()
			return struct{}{}, nil
		})
	}
	quietStep("Runner SessionDB handle sweep error", func() (struct{}, error) {
		r.closeAllSessionDBHandles()
		return struct{}{}, nil
	})
	quietStep("Shared SessionDB close error", func() (struct{}, error) {
		if closed := state.CloseAll(); closed != 0 {
			runLog().Debug(fmt.Sprintf("Closed %d shared SessionDB instance(s) at shutdown", closed))
		}
		return struct{}{}, nil
	})
	runLog().Info(fmt.Sprintf("Shutdown phase: SessionDB close done at +%.2fs", sc.elapsed()))
}

// Writes the clean shutdown marker, restart markers and the terminal runtime status
func (r *GatewayRunner) stopPersistExitState(ctx context.Context, sc *stopContext) {
	if r.startupCleanupOnly {
		shutdownGatewayHealthExport(r)
		return
	}
	if !sc.timedOut {
		func() {
			defer func() { _ = recover() }()
			path := filepath.Join(home.Process(), ".clean_shutdown")
			if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o666); err == nil {
				_ = f.Close()
				now := time.Now()
				_ = os.Chtimes(path, now, now)
			}
		}()
	} else {
		runLog().Info("Skipping .clean_shutdown marker — drain timed out with interrupted agents; " +
			"next startup will recover their interrupted turns.")
	}
	if len(sc.activeAgents) > 0 {
		r.incrementRestartFailureCounts(ctx, sortedAgentKeys(sc.activeAgents))
	}
	if r.restartRequested && r.restartCommandSource == nil {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug(fmt.Sprintf("Failed to write planned restart notification marker: %v", rec))
				}
			}()
			gateway.WritePlannedRestartMarker(ctx, r.restartViaService, r.restartDetached)
		}()
	}
	if r.restartRequested && r.restartViaService {
		code := gateway.GatewayServiceRestartExitCode
		r.exitCode = &code
		if r.exitReason == nil || *r.exitReason == "" {
			r.exitReason = optStr("Gateway restart requested")
		}
	}
	r.draining = false
	if r.signalInitiatedShutdown && !r.restartRequested {
		runLog().Info("Gateway stopped by an unexpected signal — persisting " +
			"gateway_state=running so container_boot auto-starts on the next boot (issue #42675)")
		r.updateRuntimeStatus(ctx, optStr("running"), r.exitReason)
	} else {
		r.updateRuntimeStatus(ctx, optStr("stopped"), r.exitReason)
	}
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug("Failed to flush terminal gateway runtime status", "exc_info", rec)
			}
		}()
		flushTimeout := 2.0
		if r.stopRequestedBySignal && r.launchdExitTimeoutS != nil && *r.launchdExitTimeoutS > 0 {
			flushTimeout = max(0.0, min(flushTimeout, *r.launchdExitTimeoutS-sc.elapsed()))
		}
		flushed, err := gateway.FlushRuntimeStatusAsync(ctx, time.Duration(flushTimeout*float64(time.Second)))
		if err != nil {
			runLog().Debug("Failed to flush terminal gateway runtime status", "exc_info", err)
		} else if !flushed {
			runLog().Warn("Timed out flushing terminal gateway runtime status")
		}
	}()
	shutdownGatewayHealthExport(r)
	runLog().Info(fmt.Sprintf("Gateway stopped (total teardown %.2fs)", sc.elapsed()))
}

// State the thread watchdog dumps when teardown hangs
func (r *GatewayRunner) shutdownWatchdogSnapshot(sc *stopContext) map[string]any {
	var phaseElapsed any
	if sc.startedAt != nil {
		phaseElapsed = sc.elapsed()
	}
	var launchd any
	if r.launchdExitTimeoutS != nil {
		launchd = *r.launchdExitTimeoutS
	}
	return map[string]any{
		"restart_requested": r.restartRequested, "draining": r.draining, "running": r.running,
		"active_agents": r.runningAgentCount(), "active_cron_jobs": r.activeCronJobCount(),
		"active_api_runs": r.activeAPIRunCount(), "active_deferred_agent_workers": sc.deferredCount(),
		"restart_drain_timeout": r.restartDrainTimeout, "effective_drain_timeout": gateway.EffectiveStopDrainTimeout(r.stopTiming()),
		"launchd_exit_timeout_s": launchd, "watchdog_delay_s": effectiveWatchdogLeash(r), "phase_elapsed_s": phaseElapsed,
	}
}

// Runs every stop phase under the thread based shutdown watchdog
func (r *GatewayRunner) stopImpl(ctx context.Context) error {
	watchdogDone := &pythread.Event{}
	r.shutdownWatchdogDone = watchdogDone
	sc := &stopContext{deferredCount: r.activeDeferredAgentWorkerCount}
	defer watchdogDone.Set()
	if os.Getenv("PYTEST_CURRENT_TEST") == "" {
		exitCode := 1
		gateway.ArmShutdownWatchdog(ctx, effectiveWatchdogLeash(r), gateway.ShutdownWatchdogOptions{
			Done: watchdogDone, ExitCode: &exitCode,
			SnapshotFn: func() (any, error) { return r.shutdownWatchdogSnapshot(sc), nil },
		})
	}
	if err := r.stopBeginTeardown(ctx, sc); err != nil {
		return err
	}
	timeout := gateway.EffectiveStopDrainTimeout(r.stopTiming())
	if timeout < r.restartDrainTimeout {
		launchd := 0.0
		if r.launchdExitTimeoutS != nil {
			launchd = *r.launchdExitTimeoutS
		}
		runLog().Warn(fmt.Sprintf("Shutdown drain capped to %.0fs (configured %.0fs) to fit the live launchd exit "+
			"timeout of %.0fs — launchd SIGKILLs past it", timeout, r.restartDrainTimeout, launchd))
	}
	if err := r.stopDrainActiveWork(ctx, timeout, sc); err != nil {
		return err
	}
	if sc.timedOut {
		if err := r.stopInterruptRemainingWork(ctx, sc); err != nil {
			return err
		}
	}
	reason := r.shutdownInterruptReason()
	if err := r.settleGatewayRuntime(ctx, effectiveWatchdogLeash(r)-sc.elapsed(), &reason); err != nil {
		return err
	}
	r.stopFinalizeAgentsAndAdapters(ctx, sc)
	if err := r.stopReleaseRuntimeState(ctx, sc); err != nil {
		return err
	}
	r.stopQuiesceAndCloseSessionDBs(ctx, timeout, sc)
	r.stopPersistExitState(ctx, sc)
	return nil
}

// Stops the gateway and disconnects every adapter, a second call waiting on the first
func (r *GatewayRunner) Stop(ctx context.Context, restart, detachedRestart, serviceRestart bool) error {
	r.stopLoopLivenessGuards()
	if restart {
		r.restartRequested = true
		r.restartDetached = detachedRestart
		r.restartViaService = serviceRestart
	}
	if r.stopTask != nil {
		_, err := r.stopTask.wait(ctx)
		return err
	}
	r.stopTask = spawnGatewayTask(context.WithoutCancel(ctx), "gateway-stop", func(taskCtx context.Context) (any, error) {
		return nil, r.stopImpl(taskCtx)
	})
	_, err := r.stopTask.wait(ctx)
	return err
}

// Blocks until the shutdown event is set
func (r *GatewayRunner) WaitForShutdown(ctx context.Context) error {
	for !r.shutdownEvent.Wait(0.1) {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

// Starts the restart orchestration once, refusing new turns while the requesting turn can still deliver
func (r *GatewayRunner) RequestRestart(detached, viaService bool) bool {
	if r.restartTaskStarted {
		return false
	}
	r.restartRequested = true
	r.restartDetached = detached
	r.restartViaService = viaService
	r.restartTaskStarted = true
	r.draining = true
	gateway.OpenRestartWait(r)
	r.markAPIRunsShutdownRequested()
	r.restartTask = spawnGatewayTask(context.Background(), "gateway-restart", func(ctx context.Context) (any, error) {
		if _, err := r.awaitActiveWorkBeforeRestart(ctx); err != nil {
			return nil, err
		}
		if detached {
			if err := r.launchDetachedRestartCommand(ctx); err != nil {
				logSuppressed(slog.LevelError, err, "Failed to launch detached gateway restart helper: %v")
			}
		}
		if err := sleepCtx(ctx, 0.05); err != nil {
			return nil, err
		}
		return nil, r.Stop(ctx, true, detached, viaService)
	})
	return true
}
