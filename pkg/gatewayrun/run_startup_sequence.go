package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/monitoring"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Whether a chat restart completion marker is waiting to be delivered
func restartNotificationPending() bool {
	_, err := os.Stat(filepath.Join(home.Process(), ".restart_notify.json"))
	return err == nil
}

// Creates and wires an adapter per enabled platform without connecting, aborted true when shutdown cut it short
func (r *GatewayRunner) startPrefilterPlatforms(ctx context.Context) (aborted bool, enabledCount int, skipped []gateway.Platform, pending []pendingConnect, err error) {
	multiplexOn := r.multiplexOn()
	for _, platform := range r.config.PlatformKeys() {
		platformConfig := r.config.Platforms[platform]
		stop, err := r.abortStartupIfShutdownRequested(ctx, nil, nil)
		if err != nil {
			return true, enabledCount, skipped, pending, err
		}
		if stop {
			return true, enabledCount, skipped, pending, nil
		}
		if platformConfig == nil || !platformConfig.Enabled {
			continue
		}
		if multiplexOn && !platformHasBotCredential(ctx, platform, platformConfig) {
			runLog().Info(fmt.Sprintf("Skipping %s on default profile: no bot credential in this "+
				"profile's secrets. Secondary multiplexed profiles that "+
				"provide the token will still connect.", platform))
			skipped = append(skipped, platform)
			continue
		}
		enabledCount++
		adapter, err := r.createAdapter(ctx, platform, platformConfig)
		if err != nil {
			return false, enabledCount, skipped, pending, err
		}
		if adapter == nil {
			if slices.Contains(gateway.PlatformMembers(), platform) {
				runLog().Warn(fmt.Sprintf("No adapter available for %s", platform))
			} else {
				runLog().Warn(fmt.Sprintf("No adapter for '%s' -- is the plugin installed? "+
					"(platform is enabled in config.yaml but no plugin registered it)", platform))
			}
			heals := r.adapterMayHeal(ctx, platform, platformConfig)
			r.markAdapterUnavailable(ctx, string(platform), platform, heals, optBool(true))
			if heals {
				r.failedPlatforms.Set(platform, r.startupRetryEntry(platform, nil, platformConfig, true))
			}
			continue
		}
		r.wireAdapterHandlers(ctx, adapter, wireAdapterOptions{})
		pending = append(pending, pendingConnect{platform: platform, config: platformConfig, adapter: adapter})
	}
	return false, enabledCount, skipped, pending, nil
}

// Applies the connect outcomes to shared state one by one and returns the connected adapter count
func (r *GatewayRunner) startAggregateConnectResults(ctx context.Context, raw []connectResult, retryable, nonretryable *[]string) (int, error) {
	connected := 0
	for _, item := range raw {
		if item.raised != nil {
			runLog().Error(fmt.Sprintf("Unexpected startup connect error: %v", item.raised))
			continue
		}
		platform, adapter := item.platform, item.adapter
		switch item.outcome {
		case "aborted":
			continue
		case "exception":
			runLog().Error(fmt.Sprintf("✗ %s error: %v", platform, item.exc))
			r.safeAdapterDisconnect(ctx, adapter, platform)
			r.startupQueueTransientFailure(ctx, platform, adapter, item.config, fmt.Sprint(item.exc), retryable)
			continue
		case "ok":
			if err := r.publishPrimaryAdapter(ctx, platform, adapter); err != nil {
				return connected, err
			}
			connected++
			degraded := adapter.SendPathDegraded()
			status := platformStatus{state: optStr("connected")}
			suffix := ""
			if degraded {
				status = platformStatus{state: optStr("retrying"), errorMessage: optStr(gateway.DegradedStatusMessage)}
				suffix = " (degraded)"
			}
			r.updatePlatformRuntimeStatus(ctx, string(platform), status)
			runLog().Info(fmt.Sprintf("✓ %s connected%s", platform, suffix))
			continue
		}
		runLog().Warn(fmt.Sprintf("✗ %s failed to connect", platform))
		r.safeAdapterDisconnect(ctx, adapter, platform)
		base := adapter.Base()
		if !base.HasFatalError() {
			r.startupQueueTransientFailure(ctx, platform, adapter, item.config, "failed to connect", retryable)
			continue
		}
		code := ""
		if c := base.FatalErrorCode(); c != nil {
			code = *c
		}
		conflict := gateway.IsGlobalStartupConflict(code)
		if conflict {
			r.startupOwnershipConflict = true
		}
		retry := base.FatalErrorRetryable() && !conflict
		state := "fatal"
		if retry {
			state = "retrying"
		}
		r.updatePlatformRuntimeStatus(ctx, string(platform), platformStatus{state: optStr(state), errorCode: base.FatalErrorCode(), errorMessage: base.FatalErrorMessage()})
		message := "None"
		if m := base.FatalErrorMessage(); m != nil {
			message = *m
		}
		if retry {
			*retryable = append(*retryable, fmt.Sprintf("%s: %s", platform, message))
			r.failedPlatforms.Set(platform, r.startupRetryEntry(platform, adapter, item.config, false))
		} else {
			*nonretryable = append(*nonretryable, fmt.Sprintf("%s: %s", platform, message))
		}
	}
	return connected, nil
}

// Brings up the multiplexed secondary profile adapters, aborted true on an invalid multiplexer config
func (r *GatewayRunner) startSecondaryProfiles(ctx context.Context, connected int, skipped []gateway.Platform) (bool, int) {
	tools.DefaultProcessRegistry().RestoreCompletions()
	func() {
		defer func() { r.platformLockTakeoverOnStart = false }()
		count, err := func() (count int, err error) {
			defer func() {
				if rec := recover(); rec != nil {
					err = fmt.Errorf("%v", rec)
				}
			}()
			return r.startSecondaryProfileAdapters(ctx)
		}()
		connected += count
		var configErr *MultiplexConfigError
		switch {
		case errors.As(err, &configErr):
			runLog().Error(fmt.Sprintf("Gateway multiplexer config error: %s", configErr.Error()))
			r.startupFailFatalConfig(ctx, configErr.Error())
			connected = -1 - connected
		case err != nil:
			runLog().Error(fmt.Sprintf("Secondary-profile adapter startup failed: %v", err), "exc_info", err)
		}
	}()
	if connected < 0 {
		return true, -1 - connected
	}
	for _, platform := range skipped {
		served := false
		for _, profileMap := range r.profileAdapters.Values() {
			if _, ok := profileMap.Get(platform); ok {
				served = true
				break
			}
		}
		if !served {
			runLog().Warn(fmt.Sprintf("%s is enabled but no profile (default or secondary) provided a bot credential for it — "+
				"the platform is not being served. Add its token to the profile that should "+
				"own it, or disable the platform.", platform))
		}
	}
	for _, line := range r.unservedSharedIngressWarnings() {
		runLog().Warn(line)
	}
	return false, connected
}

// Room worker, heartbeat, startup hook, channel directory and the pending update notice
func (r *GatewayRunner) startPostConnectServices(ctx context.Context, connected int) error {
	if _, err := r.ensureHostedRoomWorker(ctx); err != nil {
		runLog().Error("Group Chat worker failed to start; mutating Group Chat commands "+
			"will fail closed until supervision recovers it", "exc_info", err)
	}
	r.spawnSupervised(func(c context.Context) error { return r.hostedRoomWorkerWatcher(c, 1.0) }, "hosted_room_worker", true, 0, supervisedHooks{})
	r.startLoopHeartbeatTask(ctx)
	r.startHeartbeatPoller(ctx)
	if err := restoreHeartbeatWatches(ctx, r); err != nil {
		return err
	}
	loaded, err := r.hooks.LoadedHooks(ctx)
	if err != nil {
		return err
	}
	if len(loaded) > 0 {
		runLog().Info(fmt.Sprintf("%d hook(s) loaded", len(loaded)))
	}
	names := []any{}
	for _, p := range r.adapters.Keys() {
		names = append(names, string(p))
	}
	if err := r.hooks.emit(ctx, "gateway:startup", map[string]any{"platforms": names}); err != nil {
		return err
	}
	if connected > 0 {
		runLog().Info(fmt.Sprintf("Gateway running with %d platform(s)", connected))
	}
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				logSuppressed(slog.LevelWarn, fmt.Errorf("%v", rec), "Channel directory build failed: %s")
			}
		}()
		directory, err := gateway.BuildChannelDirectory(ctx, r.adapters)
		if err != nil {
			logSuppressed(slog.LevelWarn, err, "Channel directory build failed: %s")
			return
		}
		count := 0
		if platforms, ok := directory.GetOr("platforms", nil).(*pyjson.Object); ok && platforms != nil {
			for _, key := range platforms.Keys {
				if list, ok := platforms.Vals[key].([]any); ok {
					count += len(list)
				}
			}
		}
		runLog().Info(fmt.Sprintf("Channel directory built: %d target(s)", count))
	}()
	notified, err := r.sendUpdateNotification(ctx)
	if err != nil {
		return err
	}
	if !notified {
		for _, name := range []string{".update_pending.json", ".update_pending.claimed.json"} {
			if _, err := os.Stat(filepath.Join(home.Process(), name)); err == nil {
				r.scheduleUpdateNotificationWatch(ctx)
				break
			}
		}
	}
	return nil
}

// Post connect wiring of services, boot notifications, startup restore and recovered process watchers
func (r *GatewayRunner) startFinishWiring(ctx context.Context, connected int) error {
	if err := r.startPostConnectServices(ctx, connected); err != nil {
		return err
	}
	if connected > 0 {
		if err := sleepCtx(ctx, 1.0); err != nil {
			return err
		}
	}
	if restartNotificationPending() {
		r.bootedFromRestart = true
	}
	if err := r.awaitStartupBootSends(ctx, plannedRestartNotificationPending()); err != nil {
		return err
	}
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				logSuppressed(slog.LevelWarn, fmt.Errorf("%v", rec), "Pending-message recovery failed: %s")
			}
		}()
		_, err := gateway.RecoverGatewayPending(ctx, r.sessionStore, r.config)
		logSuppressed(slog.LevelWarn, err, "Pending-message recovery failed: %s")
	}()
	if _, err := r.scheduleResumePendingSessions(ctx, nil); err != nil {
		return err
	}
	r.finishStartupRestore(ctx)
	if _, err := recoverGatewayNativeSessions(ctx, r); err != nil {
		return err
	}
	r.sendSessionDBWarningNotifications(ctx)
	installCronStoreNotices(ctx, r, r.gatewayLoop)
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				logSuppressed(slog.LevelError, fmt.Errorf("%v", rec), "Recovered watcher setup error: %s")
			}
		}()
		for _, watcher := range tools.DefaultProcessRegistry().TakePendingWatchers() {
			w := watcher
			r.spawnSupervised(func(c context.Context) error { return r.runProcessWatcher(c, w) },
				fmt.Sprintf("process_watcher:%s", pyrepr.Str(w["session_id"])), false, 0, supervisedHooks{})
			runLog().Info(fmt.Sprintf("Resumed watcher for recovered process %s", pyrepr.Str(w["session_id"])))
		}
	}()
	return nil
}

// Spawns the long lived supervised watchers in their upstream order around the reconnect watcher
func (r *GatewayRunner) startSpawnBackgroundWatchers(ctx context.Context) {
	pre := []struct {
		name string
		run  func(context.Context) error
	}{
		{"session_housekeeping_watcher", func(c context.Context) error { return r.sessionHousekeepingWatcher(c, 300) }},
		{"model_catalog_refresh_watcher", r.modelCatalogRefreshWatcher},
		{"session_stall_watcher", func(c context.Context) error { return r.sessionStallWatcher(c, 30.0) }},
		{"kanban_notifier_watcher", func(c context.Context) error { return r.kanbanNotifierWatcher(c, 5.0) }},
		{"kanban_dispatcher_watcher", r.kanbanDispatcherWatcher},
	}
	for _, w := range pre {
		r.spawnSupervised(w.run, w.name, true, 0, supervisedHooks{})
	}
	if r.failedPlatforms.Len() > 0 {
		var names []string
		for _, p := range r.failedPlatforms.Keys() {
			names = append(names, string(p))
		}
		runLog().Info(fmt.Sprintf("Starting reconnection watcher for %d failed platform(s): %s", len(names), strings.Join(names, ", ")))
	}
	r.spawnReconnectWatcher(ctx, nil)
	post := []struct {
		name string
		run  func(context.Context) error
	}{
		{"handoff_watcher", func(c context.Context) error { return r.handoffWatcher(c, 2.0, 30.0) }},
		{"async_delegation_watcher", func(c context.Context) error { return r.asyncDelegationWatcher(c, 2.0) }},
		{"loop_wakeup_watcher", func(c context.Context) error { return r.loopWakeupWatcher(c, 15.0) }},
		{"profile_reconcile_watcher", func(c context.Context) error { return r.profileReconcileWatcher(c, profileRescanIntervalSecs) }},
	}
	for _, w := range post {
		r.spawnSupervised(w.run, w.name, true, 0, supervisedHooks{})
	}
	r.spawnSupervised(func(c context.Context) error { return runtimeDowngradeWatcher(c, r, downgradeWatch{}) }, "runtime_downgrade_watcher", true, 0, supervisedHooks{})
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug("scale-to-zero: arm check failed at startup", "exc_info", rec)
			}
		}()
		if r.scaleToZeroShouldArm(ctx) {
			idle, _ := pyrepr.Format(r.scaleToZeroIdleTimeoutSeconds(ctx), ".0f")
			runLog().Info(fmt.Sprintf("scale-to-zero: armed (idle timeout %ss) — watching for idle", idle))
			r.spawnSupervised(func(c context.Context) error { return r.scaleToZeroWatcher(c, 30.0) }, "scale_to_zero_watcher", true, 0, supervisedHooks{})
		} else {
			r.logScaleToZeroNotArmedReason(ctx)
		}
	}()
	r.spawnSupervised(func(c context.Context) error { return r.drainControlWatcher(c, 1.0) }, "drain_control_watcher", true, 0, supervisedHooks{})
}

// Bounds startup on the latest diagnostic snapshot, never on filesystem health
func startFlushRuntimeStatus(ctx context.Context) {
	ok, err := gateway.FlushRuntimeStatusAsync(ctx, 2*time.Second)
	if err != nil {
		runLog().Debug("Final startup runtime-status flush failed", "exc_info", err)
		return
	}
	if !ok {
		runLog().Warn("Timed out flushing final startup runtime status")
	}
}

// Starts the gateway and every configured platform adapter, flushing the runtime status on every path
func (r *GatewayRunner) Start(ctx context.Context) (bool, error) {
	defer startFlushRuntimeStatus(ctx)
	return r.startImpl(ctx)
}

// Startup sequence from recovery through connects to the running state and background watchers
func (r *GatewayRunner) startImpl(ctx context.Context) (bool, error) {
	runLog().Info("Starting Icebath Gateway...")
	r.startInstallFaulthandler(ctx)
	r.startLogStartupEnvironment(ctx)
	if stop, err := r.abortStartupIfShutdownRequested(ctx, nil, nil); err != nil || stop {
		return true, err
	}
	if r.startCheckAccessPolicy(ctx) {
		return true, nil
	}
	if err := r.startRecoverPreviousRun(ctx); err != nil {
		return false, err
	}
	r.startupRestoreInProgress = true
	r.startupRestoreQueue = []*gateway.MessageEvent{}
	r.startupRestoreTasks = []*gatewayTask{}
	r.startStartupWarmup(ctx)
	var nonretryable, retryable []string
	r.startupParkedPlatforms = false
	r.startupOwnershipConflict = false
	aborted, enabledCount, skipped, pending, err := r.startPrefilterPlatforms(ctx)
	if err != nil || aborted {
		return true, err
	}
	if stop, err := r.abortStartupIfShutdownRequested(ctx, nil, nil); err != nil || stop {
		return true, err
	}
	raw, err := r.startConnectPending(ctx, pending)
	if err != nil || raw == nil {
		return true, err
	}
	connected, err := r.startAggregateConnectResults(ctx, raw, &retryable, &nonretryable)
	if err != nil {
		return false, err
	}
	if stop, err := r.abortStartupIfShutdownRequested(ctx, nil, nil); err != nil || stop {
		return true, err
	}
	aborted, connected = r.startSecondaryProfiles(ctx, connected, skipped)
	if aborted {
		return true, nil
	}
	if r.startHandleNoConnections(ctx, connected, enabledCount, retryable, nonretryable) {
		return true, nil
	}
	if stop, err := r.abortStartupIfShutdownRequested(ctx, nil, nil); err != nil || stop {
		return true, err
	}
	r.deliveryRouter.Adapters = r.adapters
	r.wireTeamsPipelineRuntime(ctx)
	r.running.Store(true)
	r.installPluginMessageInjector()
	state := r.servingState()
	r.updateRuntimeStatus(ctx, &state, nil)
	if err := r.startFinishWiring(ctx, connected); err != nil {
		return false, err
	}
	r.startSpawnBackgroundWatchers(ctx)
	observability.RecordProcessReady(ctx, "gateway_boot", true)
	runLog().Info("Press Ctrl+C to stop")
	return true, nil
}

// Binds the gateway loop, disarms the startup watchdog and logs the startup environment
func (r *GatewayRunner) startLogStartupEnvironment(ctx context.Context) {
	if r.gatewayLoop != nil {
		r.startLoopLivenessGuards(ctx, r.gatewayLoop)
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug("Startup watchdog disarm failed", "exc_info", rec)
				}
			}()
			gateway.DisarmStartupWatchdog()
		}()
	}
	runLog().Info(fmt.Sprintf("Session storage: %s", r.config.SessionsDir))
	r.startLogSystemdTimingAlignment(ctx)
	r.startLogRetiredSessionReset(ctx)
	logAgentBudget()
	redactRaw, ok := os.LookupEnv("ICEBATH_REDACT_SECRETS")
	if !ok {
		redactRaw = "true"
	}
	switch strings.ToLower(redactRaw) {
	case "1", "true", "yes", "on":
		runLog().Info("Secret redaction: ENABLED (tool output, logs, and chat " +
			"responses are scrubbed before delivery)")
	default:
		runLog().Warn(fmt.Sprintf("Secret redaction: DISABLED (ICEBATH_REDACT_SECRETS=%s). API keys and tokens may appear "+
			"verbatim in chat output, session JSONs, and logs. Set security.redact_secrets: true "+
			"in config.yaml to re-enable.", redactRaw))
	}
	if profile := profiles.GetActiveProfileName(ctx); profile != "" && profile != "default" {
		runLog().Info(fmt.Sprintf("Active profile: %s", profile))
	}
	wait := 2 * time.Second
	persisted, err := gateway.WriteRuntimeStatus(ctx, gateway.RuntimeStatusUpdate{
		Fields: map[string]any{"gateway_state": "starting", "exit_reason": nil}, ClearProfilePlatforms: true,
	}, true, &wait)
	switch {
	case err != nil:
		runLog().Debug("Initial gateway runtime-status write failed", "exc_info", err)
	case !persisted:
		runLog().Warn("Timed out persisting initial gateway runtime status")
	}
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug("gateway health OTLP export startup failed", "exc_info", rec)
			}
		}()
		cfg, err := config.Load(home.Dir(ctx), nil)
		if err != nil {
			runLog().Debug("gateway health OTLP export startup failed", "exc_info", err)
			return
		}
		runtime := monitoring.StartGatewayHealthExport(map[string]any(cfg))
		r.gatewayHealthExportRuntime = runtime
		if runtime.Enabled {
			runLog().Info("Gateway health OTLP export: enabled")
		}
	}()
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug("security advisory check failed at gateway startup", "exc_info", rec)
			}
		}()
		if msg := plugins.AdvisoryGatewayLogMessage(ctx, plugins.DetectCompromised(ctx, plugins.Advisories)); msg != "" {
			runLog().Warn(msg)
			runLog().Warn("Run `icebath doctor` on the gateway host for full remediation steps.")
		}
	}()
}

// Plugins, relay and hooks, then crash or clean exit recovery of processes and sessions
func (r *GatewayRunner) startRecoverPreviousRun(ctx context.Context) error {
	startRegisterPluginsRelayHooks(ctx)
	r.startRelayProvisionRetry(ctx)
	if err := r.subscribePluginRewire(plugins.GetPluginManager(ctx), "", ""); err != nil {
		runLog().Warn("plugin re-wire subscription failed", "exc_info", err)
	}
	if err := r.hooks.discoverAndLoad(ctx); err != nil {
		return err
	}
	registry := tools.DefaultProcessRegistry()
	recovered, err := registry.RecoverFromCheckpoint(ctx, &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)})
	if err == nil {
		var secondary int
		secondary, err = r.recoverSecondaryProcessCheckpoints(ctx, registry)
		recovered += secondary
	}
	if err != nil {
		logSuppressed(slog.LevelWarn, err, "Process checkpoint recovery: %s")
	} else if recovered > 0 {
		runLog().Info(fmt.Sprintf("Recovered %d background process(es) from previous run", recovered))
	}
	if _, err := tools.RestoreUndeliveredCompletions(&tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}, registry.CompletionQueue); err != nil {
		logSuppressed(slog.LevelWarn, err, "Could not restore async delegation completions: %s")
	}
	cleanMarker := filepath.Join(home.Process(), ".clean_shutdown")
	if _, statErr := os.Stat(cleanMarker); statErr == nil {
		runLog().Info("Previous gateway exited cleanly — skipping session suspension")
		discarded, err := r.consumeCleanShutdownMarker(ctx, cleanMarker)
		if err != nil {
			runLog().Error(fmt.Sprintf("Clean-start marker cleanup failed; refusing startup so the "+
				"clean-exit receipt cannot mask a later unclean exit: %v", err))
			return fmt.Errorf("clean-start recovery cleanup failed: %w", err)
		}
		if discarded > 0 {
			runLog().Info(fmt.Sprintf("Discarded %d orphan active-turn marker(s) after clean shutdown", discarded))
		}
	} else {
		resumed, ledgered := r.recoverUncleanSessions(ctx)
		if resumed+ledgered > 0 {
			runLog().Info(fmt.Sprintf("Recovered %d interrupted turn(s) from previous run (%d to resume, %d reply(ies) "+
				"owed delivery)", resumed+ledgered, resumed, ledgered))
		}
	}
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				logSuppressed(slog.LevelDebug, fmt.Errorf("%v", rec), "Stuck-loop detection failed: %s")
			}
		}()
		if stuck := r.suspendStuckLoopSessions(ctx); stuck > 0 {
			runLog().Warn(fmt.Sprintf("Auto-suspended %d stuck-loop session(s)", stuck))
		}
	}()
	return nil
}

// Plugin discovery, relay registration and the launch profile hooks, never failing startup
func startRegisterPluginsRelayHooks(ctx context.Context) {
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Warn("plugin discovery failed at gateway startup", "exc_info", rec)
			}
		}()
		plugins.SweepPendingPluginDeletes(ctx)
		plugins.DiscoverPlugins(ctx, false)
	}()
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Warn("relay adapter registration failed at gateway startup", "exc_info", rec)
			}
		}()
		if _, err := gateway.SelfProvisionRelay(ctx); err != nil {
			runLog().Warn("relay adapter registration failed at gateway startup", "exc_info", err)
			return
		}
		registered, err := gateway.RegisterRelayAdapter(ctx)
		if err != nil {
			runLog().Warn("relay adapter registration failed at gateway startup", "exc_info", err)
			return
		}
		if registered {
			runLog().Info(fmt.Sprintf("relay adapter registered (connector at %s)", gateway.RelayURL(ctx)))
			if _, err := gateway.SendRelayPolicy(ctx); err != nil {
				runLog().Warn("relay adapter registration failed at gateway startup", "exc_info", err)
			}
		}
	}()
	registerLaunchProfileConfigHooks(ctx)
}

// Keeps provisioning the relay in the background when boot left it without credentials
func (r *GatewayRunner) startRelayProvisionRetry(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Warn("relay self-provision retry could not start", "exc_info", rec)
		}
	}()
	if gateway.RelayProvisionPending(ctx) {
		r.retainBackgroundTask(spawnGatewayTask(context.WithoutCancel(ctx), "relay-provision-retry", func(c context.Context) (any, error) {
			return gateway.RetrySelfProvision(c, r.onRelayProvisioned)
		}))
	}
}

// Pulls the queued relay retry forward once credentials arrive after boot
func (r *GatewayRunner) onRelayProvisioned() {
	if info, ok := r.failedPlatforms.Get(gateway.PlatformRelay); ok && info != nil {
		info["next_retry"] = 0.0
		info["retry_now"] = true
	}
}

// Starts the local Group Chat worker without the dashboard, an error when it has no backend or did not start
func startHostedRoomWorkerSync() (tuigateway.HostedRoomService, error) {
	service := tuigateway.GetHostedRoomService()
	if service == nil {
		started, err := tuigateway.StartHostedRoomService()
		if err != nil {
			return nil, err
		}
		service = started
	}
	if service == nil {
		return nil, errors.New("Group Chat worker has no bound session backend")
	}
	status := service.RuntimeStatus()
	if running, _ := status["running"].(bool); !running {
		return nil, errors.New("Group Chat worker did not start")
	}
	if stopping, _ := status["stopping"].(bool); stopping {
		return nil, errors.New("Group Chat worker did not start")
	}
	return service, nil
}

// Ensures the room worker runs and returns its service, through the session authority when one is bound
func (r *GatewayRunner) ensureHostedRoomWorker(ctx context.Context) (any, error) {
	if r.sessionAuthority != nil {
		return gateway.EnsureHostedService(ctx, r)
	}
	service, err := runOffLoopValue(ctx, startHostedRoomWorkerSync)
	return service, err
}

// Runs a blocking call on its own goroutine, returning its result or the context error
func runOffLoopValue[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := fn()
		done <- result{v, err}
	}()
	select {
	case res := <-done:
		return res.v, res.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// Keeps the room worker alive for the messaging gateway lifetime
func (r *GatewayRunner) hostedRoomWorkerWatcher(ctx context.Context, interval float64) error {
	for r.running.Load() {
		if _, err := r.ensureHostedRoomWorker(ctx); err != nil {
			return err
		}
		if err := sleepCtx(ctx, interval); err != nil {
			return err
		}
	}
	return nil
}
