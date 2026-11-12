package gatewayrun

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	interruptReasonGatewayShutdown     = "Gateway shutting down"
	interruptReasonGatewayRestart      = "Gateway restarting"
	interruptToolReasonGatewayShutdown = "gateway shutdown"
	executorQuiesceTimeout             = 2.0
	finalizeTimeoutS                   = 10.0
	stuckLoopThreshold                 = 3
	stuckLoopFile                      = ".restart_failure_counts"
)

// True after logging the reason when the runner asked for a failure exit
func exitWithFailureVerdict(r *GatewayRunner) bool {
	if !r.ShouldExitWithFailure() {
		return false
	}
	if reason := r.ExitReason(); reason != nil && *reason != "" {
		runLog().Error(fmt.Sprintf("Gateway exiting with failure: %s", *reason))
	}
	return true
}

// Process verdict after a startup abort or a normal shutdown, a SystemExitError carrying an exit code
func resolveGatewayExitVerdict(r *GatewayRunner, signalInitiatedShutdown bool) (bool, error) {
	if exitWithFailureVerdict(r) {
		return false, nil
	}
	if code := r.ExitCode(); code != nil {
		return false, &gateway.SystemExitError{Code: *code}
	}
	if signalInitiatedShutdown && !r.restartRequested {
		runLog().Info("Exiting with code 1 (signal-initiated shutdown without restart " +
			"request) so the service manager can revive the gateway.")
		return false, nil
	}
	if r.restartViaService {
		runLog().Info(fmt.Sprintf("Exiting with code %d (service-restart requested) so the service "+
			"manager relaunches the gateway.", gateway.GatewayServiceRestartExitCode))
		return false, &gateway.SystemExitError{Code: gateway.GatewayServiceRestartExitCode}
	}
	return true, nil
}

// Stop timing attributes of the runner as the restart helpers read them
func (r *GatewayRunner) stopTiming() gateway.StopTiming {
	drain := r.restartDrainTimeout
	return gateway.StopTiming{RestartDrainTimeout: &drain, StopRequestedBySignal: r.stopRequestedBySignal,
		LaunchdExitTimeoutS: r.launchdExitTimeoutS}
}

// Thread watchdog leash of the stop in progress, the effective drain plus grace capped under launchd
func effectiveWatchdogLeash(r *GatewayRunner) float64 {
	timing := r.stopTiming()
	return gateway.EffectiveStopWatchdogDelay(timing, gateway.ResolveShutdownWatchdogDelay(gateway.EffectiveStopDrainTimeout(timing), nil))
}

// State threaded through the phases of one stop run
type stopContext struct {
	deferredCount func() int
	startedAt     *float64
	activeAgents  map[string]*agent.AIAgent
	timedOut      bool
	drainElapsed  float64
	apiLive       int
}

// Seconds since the stop began
func (c *stopContext) elapsed() float64 {
	return pythread.Monotonic() - *c.startedAt
}

// API server adapter interrupting its own runs outside the running agent map
type apiRunInterrupter interface {
	InterruptActiveRuns(reason string) int
}

// API server adapter persisting the shutdown boundary on its runs
type apiRunShutdownMarker interface {
	MarkShutdownRequested() int
}

// API server executor threads still inside an agent turn, zero when unreadable
func (r *GatewayRunner) activeAPIWorkerCount() (n int) {
	defer func() {
		if rec := recover(); rec != nil {
			n = 0
		}
	}()
	return max(0, gateway.APIWorkerLiveCount())
}

// Interrupts API server agents outside the running agent map, zero when the hook is absent or fails
func (r *GatewayRunner) interruptAPIServerRuns(reason string) (n int) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug(fmt.Sprintf("Failed interrupting api_server runs during shutdown: %v", rec))
			n = 0
		}
	}()
	adapter, _ := r.adapters.Get(gateway.PlatformAPIServer)
	hook, ok := adapter.(apiRunInterrupter)
	if !ok {
		return 0
	}
	return max(0, hook.InterruptActiveRuns(reason))
}

// Persists the shutdown boundary on API runs before the drain can wait, zero without the hook
func (r *GatewayRunner) markAPIRunsShutdownRequested() (n int) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug(fmt.Sprintf("Failed marking api_server runs as shutdown-requested: %v", rec))
			n = 0
		}
	}()
	adapter, _ := r.adapters.Get(gateway.PlatformAPIServer)
	hook, ok := adapter.(apiRunShutdownMarker)
	if !ok {
		return 0
	}
	return max(0, hook.MarkShutdownRequested())
}

// Hard interrupts each detached executor backed agent once, returning how many were signalled
func (r *GatewayRunner) interruptDeferredAgentWorkers(reason string) int {
	r.deferredAgentWorkersMu.Lock()
	if r.deferredAgentWorkers == nil {
		r.deferredAgentWorkersMu.Unlock()
		return 0
	}
	type worker struct {
		future *gatewayFuture
		agent  *agent.AIAgent
	}
	workers := make([]worker, 0, len(r.deferredAgentWorkers))
	for future, a := range r.deferredAgentWorkers {
		workers = append(workers, worker{future, a})
	}
	r.deferredAgentWorkersMu.Unlock()
	interrupted := 0
	seen := map[*agent.AIAgent]bool{}
	for _, w := range workers {
		if w.future.isDone() || w.agent == nil || seen[w.agent] {
			continue
		}
		seen[w.agent] = true
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug(fmt.Sprintf("Failed interrupting deferred agent worker during shutdown: %v", rec))
				}
			}()
			agent.RequestHardInterrupt(w.agent, reason, interruptToolReasonGatewayShutdown)
			interrupted++
		}()
	}
	return interrupted
}

// Live background work other than permanent supervised watchers blocks a suspend
func (r *GatewayRunner) scaleToZeroHasLiveBackgroundWork() bool {
	r.taskSetsMu.Lock()
	tasks := make([]*gatewayTask, 0, len(r.backgroundTasks))
	for t := range r.backgroundTasks {
		tasks = append(tasks, t)
	}
	r.taskSetsMu.Unlock()
	for _, t := range tasks {
		if !t.isDone() && !r.isSupervisedWatcher(t) {
			return true
		}
	}
	probes := []struct {
		label string
		probe func() bool
	}{
		{"async-delegation", func() bool { return tools.AsyncDelegationActiveCount() > 0 }},
		{"bg-work", func() bool {
			registry := tools.DefaultProcessRegistry()
			return registry.HasAnyActive() || len(registry.PendingWatchers) > 0
		}},
	}
	for _, p := range probes {
		active := func() (active bool) {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug(fmt.Sprintf("scale-to-zero %s check failed", p.label), "exc_info", rec)
					active = false
				}
			}()
			return p.probe()
		}()
		if active {
			return true
		}
	}
	return false
}

// The gateway section named name of the user config when it is a mapping, nil otherwise
func gatewayCfgSection(ctx context.Context, name string) map[string]any {
	userCfg := config.EffectiveUserConfig(ctx)
	gw, _ := userCfg["gateway"].(map[string]any)
	section, _ := gw[name].(map[string]any)
	return section
}

// Configured scale to zero idle timeout in seconds
func (r *GatewayRunner) scaleToZeroIdleTimeoutSeconds(ctx context.Context) float64 {
	stz := gatewayCfgSection(ctx, "scale_to_zero")
	var value any
	if stz != nil {
		value = stz["idle_timeout_minutes"]
	}
	return gateway.ParseIdleTimeoutSeconds(value, gateway.DefaultIdleTimeoutMinutes)
}

// Max restarts, window seconds and max gap seconds of the restart loop breaker
func (r *GatewayRunner) restartLoopGuardConfig(ctx context.Context) (int, int, int) {
	rlg := gatewayCfgSection(ctx, "restart_loop_guard")
	intOr := func(key string, def int, positive bool) int {
		switch v := rlg[key].(type) {
		case int:
			if v > 0 || !positive {
				return v
			}
		case int64:
			if v > 0 || !positive {
				return int(v)
			}
		case bool:
			n := 0
			if v {
				n = 1
			}
			if n > 0 || !positive {
				return n
			}
		}
		return def
	}
	return intOr("max_restarts", gateway.RestartLoopDefaultMaxRestarts, false),
		intOr("window_seconds", gateway.RestartLoopDefaultWindowSeconds, true),
		intOr("max_gap_seconds", gateway.RestartLoopDefaultMaxGapSeconds, true)
}

// Every enabled or live messaging platform across served profiles, unavailable when unreadable
func (r *GatewayRunner) scaleToZeroActiveMessagingPlatforms() (active []string) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug("scale-to-zero: active messaging platforms unreadable — staying awake", "exc_info", rec)
			active = []string{"<unavailable>"}
		}
	}()
	nonMessaging := map[gateway.Platform]bool{gateway.PlatformLocal: true, gateway.PlatformAPIServer: true, gateway.PlatformWebhook: true}
	active = []string{}
	seen := map[gateway.Platform]bool{}
	add := func(platform gateway.Platform) {
		if !nonMessaging[platform] && !seen[platform] {
			seen[platform] = true
			active = append(active, string(platform))
		}
	}
	if r.config != nil {
		for _, platform := range r.config.PlatformKeys() {
			if cfg := r.config.Platforms[platform]; cfg != nil && cfg.Enabled {
				add(platform)
			}
		}
	}
	for platform := range r.adapters.All() {
		add(platform)
	}
	for _, profileAdapters := range r.profileAdapters.All() {
		for platform := range profileAdapters.All() {
			add(platform)
		}
	}
	for _, name := range sortedKeys(r.profileFailedPlatforms) {
		pending := make([]gateway.Platform, 0, len(r.profileFailedPlatforms[name]))
		for platform := range r.profileFailedPlatforms[name] {
			pending = append(pending, platform)
		}
		slices.Sort(pending)
		for _, platform := range pending {
			add(platform)
		}
	}
	return active
}

// Registered relay wake URL, nil when unset or unreadable
func relayWakeURLOrNone(ctx context.Context) (wake *string) {
	defer func() {
		if rec := recover(); rec != nil {
			wake = nil
		}
	}()
	return gateway.RelayWakeURL(ctx)
}

// Whether the idle watcher should start
func (r *GatewayRunner) scaleToZeroShouldArm(ctx context.Context) bool {
	return gateway.ShouldArm(gateway.ScaleToZeroEnabled(nil),
		gateway.MessagingIsRelayOnlyOrAbsent(r.scaleToZeroActiveMessagingPlatforms()), relayWakeURLOrNone(ctx))
}

// One info line on why an opted in instance did not arm the idle watcher
func (r *GatewayRunner) logScaleToZeroNotArmedReason(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug("scale-to-zero: not-armed reason logging failed", "exc_info", rec)
		}
	}()
	if !gateway.ScaleToZeroEnabled(nil) {
		return
	}
	active := r.scaleToZeroActiveMessagingPlatforms()
	enabled := "none"
	if len(active) > 0 {
		enabled = pyrepr.Repr(active)
	}
	wake := "MISSING"
	if w := relayWakeURLOrNone(ctx); w != nil && *w != "" {
		wake = "set"
	}
	runLog().Info(fmt.Sprintf("scale-to-zero: NOT armed despite opt-in — relay_only_or_absent=%s (enabled platforms=%s), "+
		"wake_url=%s. Need relay-only messaging + a registered wake URL.",
		pyrepr.Repr(gateway.MessagingIsRelayOnlyOrAbsent(active)), enabled, wake))
}

// Value of fn, the busy sentinel when the work source cannot be read so the gateway stays awake
func readOrAwake[T any](label string, fn func() T, busy T) (out T) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug(fmt.Sprintf("scale-to-zero: %s unreadable — staying awake", label), "exc_info", rec)
			out = busy
		}
	}()
	return fn()
}

// Whether the gateway is idle over the full work aggregate, an unreadable source counting as work
func (r *GatewayRunner) scaleToZeroIsIdle(ctx context.Context) bool {
	cronCount := readOrAwake("cron work count", runningCronJobCount, 1)
	apiCount := readOrAwake("api work count", r.apiServerActiveAgentWorkCount, 1)
	lastInbound := r.lastInboundAt
	now := wallClock()
	seen := readOrAwake("dashboard heartbeat", func() *float64 { return gateway.DashboardClientLastSeen("", nil) }, &now)
	if seen != nil && *seen > lastInbound {
		lastInbound = *seen
	}
	return gateway.IsIdle(r.runningAgentCount()+cronCount+apiCount, wallClock()-lastInbound,
		r.scaleToZeroIdleTimeoutSeconds(ctx), r.scaleToZeroHasLiveBackgroundWork())
}

// Stamps real inbound and restores the serving state after a dormant wake
func (r *GatewayRunner) scaleToZeroNoteRealInbound(ctx context.Context) {
	r.lastInboundAt = wallClock()
	if r.scaleToZeroCooldownUntil > 0 {
		r.scaleToZeroStatus(ctx, r.servingState(), "scale-to-zero: status restore failed")
		r.scaleToZeroCooldownUntil = 0.0
	}
}

// Writes the runtime status, a failure debug logged with failMsg
func (r *GatewayRunner) scaleToZeroStatus(ctx context.Context, state, failMsg string) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug(failMsg, "exc_info", rec)
		}
	}()
	r.updateRuntimeStatus(ctx, &state, nil)
}

// Connected relay adapter the dormant sequence targets, nil when none
func (r *GatewayRunner) relayAdapterForDormancy() gateway.PlatformAdapter {
	adapter, _ := r.adapters.Get(gateway.PlatformRelay)
	return adapter
}

// Relay adapter that quiesces its socket with the connector buffering inbound
type relayDormantAdapter interface {
	GoDormant(ctx context.Context) (bool, error)
}

// Relay adapter that holds its reconnect supervisor across a suspend
type relayRedialHolder interface {
	HoldRedial() bool
}

// Relay adapter that releases its held reconnect supervisor
type relayRedialReleaser interface {
	ReleaseRedial() bool
}

var (
	_ relayDormantAdapter = (*gateway.RelayAdapter)(nil)
	_ relayRedialHolder   = (*gateway.RelayAdapter)(nil)
	_ relayRedialReleaser = (*gateway.RelayAdapter)(nil)
)

// Watches for idle, drives the relay dormant, then suspends the machine
func (r *GatewayRunner) scaleToZeroWatcher(ctx context.Context, interval float64) error {
	if err := sleepCtx(ctx, math.Min(interval, 30.0)); err != nil {
		return err
	}
	for r.running.Load() {
		if err := r.scaleToZeroWatcherTick(ctx, interval); err != nil {
			return err
		}
	}
	return nil
}

// One watcher iteration, only cancellation escaping
func (r *GatewayRunner) scaleToZeroWatcherTick(ctx context.Context, interval float64) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug("scale-to-zero watcher iteration error", "exc_info", rec)
			err = nil
		}
	}()
	if err := sleepCtx(ctx, interval); err != nil {
		return err
	}
	if !r.running.Load() {
		return nil
	}
	if wallClock() < r.scaleToZeroCooldownUntil || !r.scaleToZeroIsIdle(ctx) {
		return nil
	}
	active := r.scaleToZeroActiveMessagingPlatforms()
	if !gateway.MessagingIsRelayOnlyOrAbsent(active) {
		if !r.scaleToZeroDirectPlatformLogged {
			r.scaleToZeroDirectPlatformLogged = true
			runLog().Info(fmt.Sprintf("scale-to-zero: idle, but directly connected messaging platform(s) %s "+
				"hold a live socket that a suspended instance cannot wake from — staying "+
				"awake. Route them through the relay connector or disable them to allow "+
				"suspend.", strings.Join(active, ", ")))
		}
		return nil
	}
	r.scaleToZeroDirectPlatformLogged = false
	dormant, ok := r.relayAdapterForDormancy().(relayDormantAdapter)
	if !ok {
		return nil
	}
	if !gateway.SuspendAvailable(nil) {
		if !r.scaleToZeroNoSuspendLogged {
			r.scaleToZeroNoSuspendLogged = true
			runLog().Info("scale-to-zero: idle, but this platform offers no suspend lever (no " +
				"in-machine API and no brokered sleep URL); staying connected rather " +
				"than quiescing")
		}
		return nil
	}
	runLog().Info(fmt.Sprintf("scale-to-zero: gateway idle for >= %.0fs — going dormant "+
		"(relay buffered, socket closed) then self-suspending", r.scaleToZeroIdleTimeoutSeconds(ctx)))
	r.scaleToZeroStatus(ctx, "draining", "scale-to-zero: status mark failed")
	if !r.scaleToZeroHoldRedial(true) {
		runLog().Warn("scale-to-zero: could not hold the relay re-dial — staying awake rather " +
			"than suspending unprotected")
		r.scaleToZeroAbandonSuspend(ctx)
		return nil
	}
	dormantOK := func() (ok bool) {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug("scale-to-zero: go_dormant failed", "exc_info", rec)
				ok = false
			}
		}()
		acked, err := dormant.GoDormant(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			runLog().Debug("scale-to-zero: go_dormant failed", "exc_info", err)
			return false
		}
		if !acked {
			runLog().Warn("scale-to-zero: connector did not ack going_idle — staying awake " +
				"rather than freezing a live destination")
			return false
		}
		return true
	}()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	r.scaleToZeroCooldownUntil = wallClock() + math.Max(interval, 60.0)
	if !dormantOK {
		r.scaleToZeroAbandonSuspend(ctx)
		return nil
	}
	if !r.scaleToZeroIsIdle(ctx) {
		runLog().Info("scale-to-zero: inbound arrived during quiesce — skipping suspend")
		r.scaleToZeroAbandonSuspend(ctx)
		return nil
	}
	return r.scaleToZeroSelfSuspend(ctx)
}

// Suspends this machine in guest where possible and through NAS otherwise, staying awake on any failure
func (r *GatewayRunner) scaleToZeroSelfSuspend(ctx context.Context) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug("scale-to-zero: self-suspend failed", "exc_info", rec)
			r.scaleToZeroAbandonSuspend(ctx)
			err = nil
		}
	}()
	var accepted bool
	var lever string
	if gateway.SelfSuspendAvailable(nil) {
		accepted = gateway.SuspendSelf(nil, gateway.FlyAPISocket, 10.0)
		lever = "self-suspend"
		if accepted {
			if err := r.scaleToZeroAwaitFreezeGap(ctx); err != nil {
				return err
			}
			r.scaleToZeroHoldRedial(false)
		} else {
			r.scaleToZeroAbandonSuspend(ctx)
		}
	} else {
		url := gateway.BrokeredSleepURL(nil)
		if url == nil {
			r.scaleToZeroAbandonSuspend(ctx)
			runLog().Debug("scale-to-zero: no suspend lever available — dormant without platform suspend")
			return nil
		}
		accepted = gateway.RequestBrokeredSuspend(ctx, *url, gateway.BrokeredSuspendTimeoutS, nil)
		lever = "brokered suspend"
		if !accepted {
			r.scaleToZeroAbandonSuspend(ctx)
		}
	}
	if !accepted {
		runLog().Warn(fmt.Sprintf("scale-to-zero: %s not accepted — machine stays awake (fail-awake); will "+
			"retry on the next idle window", lever))
	}
	return nil
}

// Holds the re-dial fence across the flaps answer to kernel freeze gap, sliced on the wall clock
func (r *GatewayRunner) scaleToZeroAwaitFreezeGap(ctx context.Context) error {
	deadline := wallClock() + gateway.FlyFreezeGraceS
	for wallClock() < deadline {
		if err := sleepCtx(ctx, gateway.FlyFreezeGraceTickS); err != nil {
			return err
		}
	}
	return nil
}

// Undoes a quiesce no suspend follows, releasing the hold, restoring status and setting a cooldown
func (r *GatewayRunner) scaleToZeroAbandonSuspend(ctx context.Context) {
	r.scaleToZeroHoldRedial(false)
	if !r.draining.Load() && r.running.Load() {
		r.scaleToZeroStatus(ctx, r.servingState(), "scale-to-zero: status restore failed")
	}
	r.scaleToZeroCooldownUntil = math.Max(r.scaleToZeroCooldownUntil, wallClock()+60.0)
}

// Holds or releases the relay reconnect supervisor, true only when the transport took it
func (r *GatewayRunner) scaleToZeroHoldRedial(held bool) (took bool) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug("scale-to-zero: redial hold toggle failed", "exc_info", rec)
			took = false
		}
	}()
	adapter := r.relayAdapterForDormancy()
	if adapter == nil {
		return false
	}
	if held {
		holder, ok := adapter.(relayRedialHolder)
		return ok && holder.HoldRedial()
	}
	releaser, ok := adapter.(relayRedialReleaser)
	return ok && releaser.ReleaseRedial()
}

// Begins external drain, refusing new turns while in flight ones finish
func (r *GatewayRunner) enterExternalDrain(ctx context.Context) {
	if r.externalDrainActive {
		return
	}
	r.externalDrainActive = true
	runLog().Info(fmt.Sprintf("External drain ENGAGED (.drain_request.json present) — refusing "+
		"new turns; %d in-flight turn(s) will finish. Process stays up.", r.activeWorkCount()))
	r.updateRuntimeStatus(ctx, optStr("draining"), nil)
}

// Cancels external drain and re-accepts new turns, never resurrecting a stopping gateway
func (r *GatewayRunner) exitExternalDrain(ctx context.Context) {
	if !r.externalDrainActive {
		return
	}
	r.externalDrainActive = false
	if r.draining.Load() || !r.running.Load() {
		runLog().Info("External drain marker cleared during shutdown — not reverting " +
			"to running (shutdown takes precedence).")
		return
	}
	runLog().Info(fmt.Sprintf("External drain RELEASED (.drain_request.json removed) — "+
		"re-accepting new turns; gateway_state -> %s.", r.servingState()))
	r.updateRuntimeStatus(ctx, optStr(r.servingState()), nil)
}

// Polls the drain marker every interval, entering drain while present and leaving it once absent
func (r *GatewayRunner) drainControlWatcher(ctx context.Context, interval float64) error {
	for r.running.Load() {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug(fmt.Sprintf("Drain-control watcher tick error: %v", rec), "exc_info", rec)
				}
			}()
			requested := make(chan bool, 1)
			go func() { requested <- gateway.DrainRequested(ctx, home.Dir(ctx)) }()
			select {
			case <-ctx.Done():
				return
			case present := <-requested:
				if present {
					r.enterExternalDrain(ctx)
					r.persistActiveAgents(ctx)
				} else {
					r.exitExternalDrain(ctx)
				}
			}
		}()
		if err := sleepCtx(ctx, interval); err != nil {
			return err
		}
	}
	return nil
}

// Pauses a queued platform with the reason, a manual platform pause only
func (r *GatewayRunner) pauseFailedPlatform(ctx context.Context, platform gateway.Platform, reason string) {
	info, ok := r.failedPlatforms.Get(platform)
	if !ok || info == nil || pyrepr.Truthy(info["paused"]) {
		return
	}
	info["paused"] = true
	if reason == "" {
		reason = "auto-paused after repeated failures"
	}
	info["pause_reason"] = reason
	info["next_retry"] = math.Inf(1)
	r.updatePlatformRuntimeStatus(ctx, string(platform), platformStatus{state: optStr("paused"), errorMessage: optStr(reason)})
	attempts := 0
	if n, ok := info["attempts"].(int); ok {
		attempts = n
	}
	runLog().Warn(fmt.Sprintf("%s paused after %d consecutive failures (%s) — fix the underlying issue then run `/platform "+
		"resume %s` to retry, or `icebath gateway restart` to restart the gateway.", platform, attempts, reason, platform))
}

// Unpauses a platform for a retry on the next watcher tick, true when it was paused
func (r *GatewayRunner) resumePausedPlatform(ctx context.Context, platform gateway.Platform) bool {
	info, ok := r.failedPlatforms.Get(platform)
	if !ok || info == nil || !pyrepr.Truthy(info["paused"]) {
		return false
	}
	info["paused"] = false
	delete(info, "pause_reason")
	info["attempts"] = 0
	info["next_retry"] = pythread.Monotonic()
	r.updatePlatformRuntimeStatus(ctx, string(platform), platformStatus{state: optStr("retrying")})
	runLog().Info(fmt.Sprintf("%s resumed — retrying on next watcher tick", platform))
	return true
}
