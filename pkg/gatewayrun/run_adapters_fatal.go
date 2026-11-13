package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/observability"
)

// Runs the fatal handler detached and shielded so the failing adapter's own task cannot cancel it mid flight
func (r *GatewayRunner) handleAdapterFatalError(ctx context.Context, adapter gateway.PlatformAdapter) error {
	r.taskSetsMu.Lock()
	if r.fatalHandlerTasks == nil {
		r.fatalHandlerTasks = map[*gatewayTask]struct{}{}
	}
	tasks := r.fatalHandlerTasks
	r.taskSetsMu.Unlock()
	task := r.trackTaskIn(tasks, spawnGatewayTask(context.WithoutCancel(ctx), "adapter-fatal-handler", func(c context.Context) (any, error) {
		return nil, r.handleAdapterFatalErrorDetached(c, adapter)
	}))
	_, err := task.wait(ctx)
	return err
}

// Queues a retryable fatal adapter for background reconnection without awaiting, true when newly queued
func (r *GatewayRunner) queueRetryableFatalPlatform(ctx context.Context, adapter gateway.PlatformAdapter) bool {
	base := adapter.Base()
	if !base.FatalErrorRetryable() {
		return false
	}
	var platformConfig *gateway.PlatformConfig
	if r.config != nil {
		platformConfig = r.config.Platforms[base.Platform]
	}
	if platformConfig == nil {
		return false
	}
	if r.failedPlatforms.Has(base.Platform) {
		r.ensureReconnectWatcherRunning(ctx)
		return false
	}
	r.failedPlatforms.Set(base.Platform, r.reconnectQueueEntry(base.Platform, adapter, platformConfig, 0, 0, true))
	runLog().Info(fmt.Sprintf("%s queued for background reconnection", base.Platform))
	r.ensureReconnectWatcherRunning(ctx)
	return true
}

// Runs the fatal handler and exits with failure when it left a retryable platform stranded
func (r *GatewayRunner) handleAdapterFatalErrorDetached(ctx context.Context, adapter gateway.PlatformAdapter) (err error) {
	base := adapter.Base()
	defer func() {
		platform := base.Platform
		if base.FatalErrorRetryable() && !r.adapters.Has(platform) && !r.failedPlatforms.Has(platform) &&
			!(r.shutdownEvent != nil && r.shutdownEvent.IsSet()) {
			if r.onAllAdaptersDown() == "stay_alive" {
				runLog().Warn(fmt.Sprintf("%s adapter was lost without entering the reconnection queue; "+
					"on_all_adapters_down=stay_alive — gateway staying alive, reconnect "+
					"watcher owns recovery.", platform))
				r.ensureReconnectWatcherRunning(ctx)
				return
			}
			runLog().Error(fmt.Sprintf("%s adapter was lost without entering the reconnection "+
				"queue; exiting gateway so the service manager restarts it.", platform))
			reason := fmt.Sprintf("%s adapter lost without reconnection queue", platform)
			r.exitReason = &reason
			r.exitWithFailure = true
			if stopErr := r.Stop(ctx, false, false, false); stopErr != nil && err == nil {
				err = stopErr
			}
		}
	}()
	raised := func() (raised error) {
		defer func() {
			if rec := recover(); rec != nil {
				raised = fmt.Errorf("%v", rec)
			}
		}()
		timeout := r.adapterDisconnectTimeoutSecs()
		if timeout <= 0 {
			return r.handleAdapterFatalErrorImpl(ctx, adapter)
		}
		outer := timeout + min(2.0, max(0.05, timeout))
		finished, err := r.awaitAdapterCleanupWithTimeout(ctx, func(c context.Context) error {
			return r.handleAdapterFatalErrorImpl(c, adapter)
		}, outer)
		if err != nil {
			return err
		}
		if !finished {
			runLog().Error(fmt.Sprintf("Fatal-error handling for %s timed out after %.1fs; "+
				"ensuring reconnect queue is populated", base.Platform, outer))
			r.queueRetryableFatalPlatform(ctx, adapter)
		}
		return nil
	}()
	if raised == nil {
		return nil
	}
	if isCancellation(raised) || ctx.Err() != nil {
		r.queueRetryableBestEffort(ctx, adapter, "cancellation")
		return raised
	}
	runLog().Error(fmt.Sprintf("Fatal-error handling for %s raised unexpectedly", base.Platform), "exc_info", raised)
	r.queueRetryableBestEffort(ctx, adapter, "exception")
	return nil
}

// Queues a retryable platform after a failed fatal handler, a raise only logged
func (r *GatewayRunner) queueRetryableBestEffort(ctx context.Context, adapter gateway.PlatformAdapter, why string) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug(fmt.Sprintf("Failed to queue %s after fatal-handler %s", adapter.Base().Platform, why), "error", rec)
		}
	}()
	r.queueRetryableFatalPlatform(ctx, adapter)
}

// Text of an optional adapter error field, the fallback when unset or empty
func orText(s *string, fallback string) string {
	if s == nil || *s == "" {
		return fallback
	}
	return *s
}

// Tears down a fatal primary adapter, queues it for reconnection and stops the gateway once nothing can serve
func (r *GatewayRunner) handleAdapterFatalErrorImpl(ctx context.Context, adapter gateway.PlatformAdapter) error {
	base := adapter.Base()
	platform := base.Platform
	existing, live := r.adapters.Get(platform)
	if live && existing != nil && existing != adapter {
		runLog().Debug(fmt.Sprintf("Ignoring stale fatal error from a superseded %s adapter instance: %s",
			platform, orText(base.FatalErrorCode(), "unknown")))
		return nil
	}
	runLog().Error(fmt.Sprintf("Fatal %s adapter error (%s): %s", platform,
		orText(base.FatalErrorCode(), "unknown"), orText(base.FatalErrorMessage(), "unknown error")))
	state := "fatal"
	if code := base.FatalErrorCode(); code != nil && *code == "relay_disabled" {
		state = "disabled"
	} else if base.FatalErrorRetryable() {
		state = "retrying"
	}
	r.updatePlatformRuntimeStatus(ctx, string(platform), platformStatus{state: optStr(state),
		errorCode: base.FatalErrorCode(), errorMessage: base.FatalErrorMessage()})
	owned := live && existing == adapter
	if owned {
		observability.RecordPlatformDisconnect(ctx, adapter, "")
		r.adapters.Delete(platform)
		r.deliveryRouter.Adapters = r.adapters
	}
	r.queueRetryableFatalPlatform(ctx, adapter)
	if owned {
		r.safeAdapterDisconnect(ctx, adapter, platform)
	}
	switch {
	case r.adapters.Len() == 0 && r.failedPlatforms.Len() == 0:
		if base.FatalErrorRetryable() && r.onAllAdaptersDown() == "stay_alive" {
			runLog().Warn(fmt.Sprintf("No connected messaging platforms remain; on_all_adapters_down=stay_alive — "+
				"gateway staying alive, reconnect watcher owns recovery of %s.", platform))
			r.ensureReconnectWatcherRunning(ctx)
			return nil
		}
		if r.parkLastFatalPlatform(ctx, adapter) {
			return nil
		}
		reason := orText(base.FatalErrorMessage(), "All messaging adapters disconnected")
		r.exitReason = &reason
		if base.FatalErrorRetryable() {
			r.exitWithFailure = true
			runLog().Error("No connected messaging platforms remain. Shutting down gateway for service restart.")
		} else {
			runLog().Error("No connected messaging platforms remain. Shutting down gateway cleanly.")
		}
		return r.Stop(ctx, false, false, false)
	case r.adapters.Len() == 0:
		runLog().Warn(fmt.Sprintf("No connected messaging platforms remain, but %d platform(s) "+
			"queued for reconnection — gateway staying alive, watcher will "+
			"retry in background.", r.failedPlatforms.Len()))
	}
	return nil
}

// Keeps a respawn of the reconnect watcher scheduled once supervision gave up while platforms stay queued
func (r *GatewayRunner) onReconnectWatcherGaveUp(ctx context.Context, name string) {
	if !r.running.Load() {
		return
	}
	if r.failedPlatforms.Len() > 0 {
		r.scheduleSlowReconnectWatcherRespawn(ctx, 0)
		return
	}
	runLog().Warn("Reconnect watcher supervision exhausted with an empty retry " +
		"queue — leaving it down until a platform is queued.")
}

// Bounded slow tier respawn of the reconnect watcher
func (r *GatewayRunner) scheduleSlowReconnectWatcherRespawn(ctx context.Context, attempt int) {
	if attempt >= r.maxSlowWatcherRespawns {
		var names []string
		for _, p := range r.failedPlatforms.Keys() {
			names = append(names, string(p))
		}
		runLog().Error(fmt.Sprintf("Reconnect watcher could not be kept alive after %d slow respawns; %d platform(s) remain "+
			"queued and unattended: %s. Manual intervention or a gateway restart is required.",
			attempt, r.failedPlatforms.Len(), strings.Join(names, ", ")))
		return
	}
	r.retainBackgroundTask(spawnGatewayTask(context.WithoutCancel(ctx), "reconnect-watcher-slow-respawn", func(c context.Context) (any, error) {
		if err := sleepCtx(c, r.reconnectWatcherSlowRetrySecs); err != nil {
			return nil, err
		}
		task := r.reconnectWatcherTask
		if !r.running.Load() || r.failedPlatforms.Len() == 0 || (task != nil && !task.isDone()) {
			return nil, nil
		}
		runLog().Warn(fmt.Sprintf("Reconnect watcher still down with %d platform(s) queued — slow respawn %d/%d",
			r.failedPlatforms.Len(), attempt+1, r.maxSlowWatcherRespawns))
		r.spawnReconnectWatcher(c, func(string) { r.scheduleSlowReconnectWatcherRespawn(c, attempt+1) })
		return nil, nil
	}))
}

// Launches the supervised reconnect watcher, tracking every respawn so the liveness check sees the live task
func (r *GatewayRunner) spawnReconnectWatcher(ctx context.Context, onGiveUp func(name string)) *gatewayTask {
	if onGiveUp == nil {
		detached := context.WithoutCancel(ctx)
		onGiveUp = func(name string) { r.onReconnectWatcherGaveUp(detached, name) }
	}
	r.reconnectWatcherTask = r.spawnSupervised(r.platformReconnectWatcher, "platform_reconnect_watcher", true, 0,
		supervisedHooks{onSpawn: func(t *gatewayTask) { r.reconnectWatcherTask = t }, onGiveUp: onGiveUp})
	return r.reconnectWatcherTask
}

// Respawns a dead reconnect watcher while the runner is live
func (r *GatewayRunner) ensureReconnectWatcherRunning(ctx context.Context) {
	task := r.reconnectWatcherTask
	if !r.running.Load() || (task != nil && !task.isDone()) {
		return
	}
	done := "N/A"
	if task != nil {
		done = "True"
	}
	runLog().Warn(fmt.Sprintf("Reconnect watcher task is dead (done=%s) — respawning", done))
	r.spawnReconnectWatcher(ctx, nil)
}

// Sleeps in one second steps, false once the runner stops, ending early when asked and work is queued
func (r *GatewayRunner) reconnectIdle(ctx context.Context, seconds int, untilQueued bool) (bool, error) {
	for range seconds {
		if !r.running.Load() {
			return false, nil
		}
		if untilQueued && r.failedPlatforms.Len() > 0 {
			break
		}
		if err := sleepCtx(ctx, 1); err != nil {
			return false, err
		}
	}
	return true, nil
}

// Retries failed platforms with capped backoff until the runner stops
func (r *GatewayRunner) platformReconnectWatcher(ctx context.Context) error {
	if err := sleepCtx(ctx, 10); err != nil {
		return err
	}
	for r.running.Load() {
		if r.failedPlatforms.Len() == 0 {
			keep, err := r.reconnectIdle(ctx, 30, true)
			if err != nil || !keep {
				return err
			}
			continue
		}
		now := pythread.Monotonic()
		for _, platform := range r.failedPlatforms.Keys() {
			if !r.running.Load() {
				return nil
			}
			if err := r.reconnectFailedPlatform(ctx, platform, now); err != nil {
				return err
			}
		}
		keep, err := r.reconnectIdle(ctx, 10, false)
		if err != nil || !keep {
			return err
		}
	}
	return nil
}

// One watcher pass for a queued platform, gating, attempting and recording the outcome
func (r *GatewayRunner) reconnectFailedPlatform(ctx context.Context, platform gateway.Platform, now float64) error {
	info, ok := r.failedPlatforms.Get(platform)
	if !ok || info == nil {
		return nil
	}
	if paused, _ := info["paused"].(bool); paused {
		return nil
	}
	r.flagReconnectNeedsAttention(ctx, platform, info, now, "")
	if nextRetry, _ := info["next_retry"].(float64); now < nextRetry {
		return nil
	}
	platformConfig, _ := info["config"].(*gateway.PlatformConfig)
	attempts, _ := info["attempts"].(int)
	attempt := attempts + 1
	delete(info, "retry_now")
	if !platformHasBotCredential(ctx, platform, platformConfig) {
		r.dropFromReconnectQueue(platform, "no bot credential on queued config")
		return nil
	}
	runLog().Info(fmt.Sprintf("Reconnecting %s (attempt %d)...", platform, attempt))
	predecessor := info["predecessor"]
	var adapter gateway.PlatformAdapter
	err := func() (err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		if _, err := r.runInExecutorWithContext(ctx, func(c context.Context) (any, error) {
			return gateway.Registry.Get(c, string(platform)), nil
		}); err != nil {
			return err
		}
		created, err := r.createAdapter(ctx, platform, platformConfig)
		if err != nil {
			return err
		}
		adapter = created
		if adapter == nil {
			if !r.adapterMayHeal(ctx, platform, platformConfig) {
				r.markAdapterUnavailable(ctx, string(platform), platform, false, optBool(true))
				r.dropFromReconnectQueue(platform, "adapter creation returned None")
				return nil
			}
			if err := r.rearmFailedPluginLoad(ctx, platform, info); err != nil {
				return err
			}
			backoff := r.bumpReconnectBackoff(ctx, platform, info, attempt, optStr("adapter_unavailable"), adapterUnavailableMessage(platform, true))
			runLog().Info(fmt.Sprintf("Reconnect %s: no adapter yet, next retry in %ds", platform, int(backoff)))
			return nil
		}
		gateway.CarryInboundDedup(predecessor, adapter)
		r.wireAdapterHandlers(ctx, adapter, wireAdapterOptions{})
		success, err := r.connectAdapterWithTimeout(ctx, adapter, platform, true, false)
		if err != nil {
			return err
		}
		base := adapter.Base()
		switch {
		case success:
			return r.installReconnectedAdapter(ctx, platform, adapter)
		case base.HasFatalError() && !base.FatalErrorRetryable():
			r.markPlatformFatal(ctx, string(platform), adapter)
			runLog().Warn(fmt.Sprintf("Reconnect %s: non-retryable error (%s), removing from retry queue",
				platform, pyrepr.Str(optAny(base.FatalErrorMessage()))))
			if err := disposeUnusedAdapter(ctx, adapter); err != nil {
				return err
			}
			r.failedPlatforms.Delete(platform)
		default:
			message := orText(base.FatalErrorMessage(), "failed to reconnect")
			backoff := r.bumpReconnectBackoff(ctx, platform, info, attempt, base.FatalErrorCode(), message)
			runLog().Info(fmt.Sprintf("Reconnect %s failed, next retry in %ds", platform, int(backoff)))
			if err := disposeUnusedAdapter(ctx, adapter); err != nil {
				return err
			}
			gateway.HandOverHeldInbound(ctx, adapter, predecessor)
		}
		return nil
	}()
	if err == nil {
		return nil
	}
	if isCancellation(err) || ctx.Err() != nil {
		return err
	}
	if adapter != nil {
		if derr := disposeUnusedAdapter(ctx, adapter); derr != nil && (isCancellation(derr) || ctx.Err() != nil) {
			return derr
		}
		if live, _ := r.adapters.Get(platform); live != adapter {
			gateway.HandOverHeldInbound(ctx, adapter, predecessor)
		}
	}
	backoff := r.bumpReconnectBackoff(ctx, platform, info, attempt, nil, err.Error())
	runLog().Warn(fmt.Sprintf("Reconnect %s error: %s, next retry in %ds", platform, err.Error(), int(backoff)))
	return nil
}

// Registers a connected primary adapter and wires its voice mode and voice input
func (r *GatewayRunner) publishPrimaryAdapter(ctx context.Context, platform gateway.Platform, adapter gateway.PlatformAdapter) error {
	r.adapters.Set(platform, adapter)
	if platform == gateway.PlatformAPIServer {
		if err := gateway.RecoverAPITurns(ctx, adapter); err != nil {
			return err
		}
	}
	r.syncVoiceModeStateToAdapter(ctx, adapter)
	r.bindVoiceInputCallback(ctx, adapter)
	return nil
}

// Replays the owed planned restart notice in the background after a reconnect
func (r *GatewayRunner) schedulePlannedRestartReplay(ctx context.Context) {
	if !plannedRestartNotificationPending() {
		return
	}
	task := r.retainBackgroundTask(spawnGatewayTask(context.WithoutCancel(ctx), "planned-restart-replay", func(c context.Context) (any, error) {
		r.replayPendingPlannedRestartNotification(c)
		return nil, nil
	}))
	task.addDoneCallback(lateFailureCallback("planned-restart notification replay failed", slog.LevelWarn))
}

// Publishes a reconnected primary adapter and replays what it missed while down
func (r *GatewayRunner) installReconnectedAdapter(ctx context.Context, platform gateway.Platform, adapter gateway.PlatformAdapter) error {
	if err := r.publishPrimaryAdapter(ctx, platform, adapter); err != nil {
		return err
	}
	r.deliveryRouter.Adapters = r.adapters
	entry, _ := r.failedPlatforms.Get(platform)
	r.failedPlatforms.Delete(platform)
	gateway.HandOverHeldInbound(ctx, entry["predecessor"], adapter)
	degraded := adapter.SendPathDegraded()
	status := platformStatus{state: optStr("connected"), needsAttention: optBool(false), retryingSince: optSince(nil)}
	if degraded {
		status.state, status.errorMessage = optStr("retrying"), optStr(gateway.DegradedStatusMessage)
	}
	r.updatePlatformRuntimeStatus(ctx, string(platform), status)
	if degraded {
		runLog().Info(fmt.Sprintf("⚠ %s reconnected in degraded mode (receive path not yet confirmed)", platform))
	} else {
		runLog().Info(fmt.Sprintf("✓ %s reconnected successfully", platform))
	}
	r.schedulePlannedRestartReplay(ctx)
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug(fmt.Sprintf("failed-obligation redelivery after %s reconnect failed", platform), "error", rec)
			}
		}()
		r.redeliverFailedObligationsForPlatform(ctx, platform, nil)
	}()
	func() {
		defer func() { _ = recover() }()
		_, _ = gateway.BuildChannelDirectory(ctx, r.adapters)
	}()
	if _, err := r.scheduleResumePendingSessions(ctx, &platform); err != nil {
		runLog().Debug(fmt.Sprintf("resume-pending reschedule after %s reconnect failed", platform), "error", err)
	}
	return nil
}
