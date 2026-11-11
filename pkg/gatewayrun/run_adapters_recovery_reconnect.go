package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Says why a secondary platform built no adapter at startup, stamps its status and queues it when it can heal
func (r *GatewayRunner) noteUnbuiltSecondaryPlatform(ctx context.Context, profileName string, platform gateway.Platform, heals bool) {
	if heals {
		runLog().Warn(fmt.Sprintf("[MULTIPLEX] Profile '%s': no plugin registered platform '%s' - is the plugin installed? (a load "+
			"that failed or overran its deadline also leaves it unregistered); retrying in the background", profileName, platform))
	} else {
		runLog().Warn(fmt.Sprintf("[MULTIPLEX] Profile '%s': skipping platform '%s' - adapter creation returned None", profileName, platform))
	}
	r.markAdapterUnavailable(ctx, fmt.Sprintf("%s:%s", profileName, platform), platform, heals, optBool(true))
	if heals {
		r.scheduleSecondaryProfileStartupReconnect(ctx, profileName, platform, nil)
	}
}

// Profile scope of a secondary reconnect with secrets already hydrated off the loop
func profileScopeNoHydrate(ctx context.Context, home string) context.Context {
	return gateway.ProfileRuntimeScope(ctx, home, false)
}

// One scoped attempt to rebuild and connect a secondary adapter, nil success meaning give up for good
func (r *GatewayRunner) secondaryReconnectAttempt(ctx context.Context, profileName string, platform gateway.Platform, predecessor gateway.PlatformAdapter) (gateway.PlatformAdapter, *bool, error) {
	profileHome, err := profiles.GetProfileDir(profileName)
	if err != nil {
		return nil, nil, err
	}
	provider.HydrateProfileSecretSources(profileHome)
	scoped := profileScopeNoHydrate(ctx, profileHome)
	cfg, err := gateway.LoadGatewayConfig(scoped)
	if err != nil {
		return nil, nil, err
	}
	profileConfig := cfg.Platforms[platform]
	if profileConfig == nil || !profileConfig.Enabled {
		if predecessor == nil {
			r.standDownUnbuiltSecondary(scoped, profileName, platform, "disabled in config")
		}
		return nil, nil, nil
	}
	if !platformHasBotCredential(scoped, platform, profileConfig) {
		runLog().Info(fmt.Sprintf("Secondary %s reconnect skipped: no bot credential (profile: %s)", platform, profileName))
		if predecessor == nil {
			r.standDownUnbuiltSecondary(scoped, profileName, platform, "no bot credential configured")
		}
		return nil, nil, nil
	}
	if _, err := r.runInExecutorWithContext(scoped, func(c context.Context) (any, error) {
		return gateway.Registry.Get(c, string(platform)), nil
	}); err != nil {
		return nil, nil, err
	}
	adapter, err := r.createAdapter(scoped, platform, profileConfig)
	if err != nil {
		return nil, nil, err
	}
	if adapter == nil {
		if r.adapterMayHeal(scoped, platform, profileConfig) {
			runLog().Info(fmt.Sprintf("Secondary %s: no adapter yet (profile: %s)", platform, profileName))
			return nil, optBool(false), nil
		}
		runLog().Warn(fmt.Sprintf("Secondary %s reconnect skipped: adapter unavailable (profile: %s)", platform, profileName))
		r.markAdapterUnavailable(scoped, fmt.Sprintf("%s:%s", profileName, platform), platform, false, optBool(true))
		return nil, nil, nil
	}
	current := currentGatewayTask(ctx)
	if predecessor == nil {
		claims, ok := r.claimSecondaryPlatform(scoped, profileName, profileHome, platform, adapter,
			r.currentClaimOwners(&secondaryKey{profile: profileName, platform: platform}))
		if !ok {
			return nil, nil, nil
		}
		r.holdSecondaryClaims(profileName, platform, current, claims)
	} else {
		r.holdSecondaryClaims(profileName, platform, current, r.adapterClaims(platform, adapter))
	}
	gateway.CarryInboundDedup(predecessor, adapter)
	success, err := func() (success bool, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		r.configureProfileAdapter(scoped, adapter, profileName, platform)
		return r.connectAdapterWithTimeout(scoped, adapter, platform, true, false)
	}()
	if err != nil {
		r.safeAdapterDisconnect(scoped, adapter, platform)
		gateway.HandOverHeldInbound(adapter, predecessor)
		return nil, nil, err
	}
	return adapter, &success, nil
}

// Whether a reconnect that installed nothing ends, dropping bot ownership of a platform never built
func (r *GatewayRunner) endSecondaryReconnect(profileName string, platform gateway.Platform, predecessor, adapter gateway.PlatformAdapter, success *bool) bool {
	var final bool
	switch {
	case adapter == nil:
		final = success == nil
	case success != nil && *success:
		return true
	default:
		base := adapter.Base()
		final = base.HasFatalError() && !base.FatalErrorRetryable()
	}
	if final && predecessor == nil {
		r.disownSecondaryPlatform(profileName, platform)
	}
	return final
}

// Adapter map of a served profile, created on first use
func (r *GatewayRunner) profileAdapterMap(profileName string) *pydict.Dict[gateway.Platform, gateway.PlatformAdapter] {
	if r.profileAdapters == nil {
		r.profileAdapters = pydict.New[string, *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]]()
	}
	profileMap, ok := r.profileAdapters.Get(profileName)
	if !ok {
		profileMap = pydict.New[gateway.Platform, gateway.PlatformAdapter]()
		r.profileAdapters.Set(profileName, profileMap)
	}
	return profileMap
}

// Installs a reconnected secondary adapter and replays what it missed, true when it took the slot
func (r *GatewayRunner) installSecondaryReconnect(ctx context.Context, profileName string, platform gateway.Platform, predecessor, adapter gateway.PlatformAdapter) bool {
	profileMap := r.profileAdapterMap(profileName)
	if profileMap.Has(platform) {
		return false
	}
	profileMap.Set(platform, adapter)
	gateway.HandOverHeldInbound(predecessor, adapter)
	r.syncVoiceModeStateToAdapter(ctx, adapter)
	runLog().Info(fmt.Sprintf("✓ %s reconnected (profile: %s)", platform, profileName))
	profile := profileName
	r.redeliverFailedObligationsForPlatform(ctx, platform, &profile)
	r.schedulePlannedRestartReplay(ctx)
	if _, err := r.scheduleResumePendingSessions(ctx, &platform); err != nil {
		runLog().Debug(fmt.Sprintf("resume-pending reschedule after %s reconnect failed (profile: %s)", platform, profileName), "error", err)
	}
	return true
}

// Reconnects a retryable secondary adapter under its own profile scope until it is installed or gives up
func (r *GatewayRunner) runSecondaryProfileReconnect(ctx context.Context, profileName string, platform gateway.Platform, predecessor gateway.PlatformAdapter) error {
	attempts := 0
	queueInfo := map[string]any{"queued_at": pythread.Monotonic(), "attempts": 0}
	current := currentGatewayTask(ctx)
	defer func() {
		r.profileFailedMu.Lock()
		defer r.profileFailedMu.Unlock()
		profilePending, ok := r.profileFailedPlatforms[profileName]
		if !ok || profilePending == nil {
			return
		}
		if task := profilePending[platform]; task == nil || task == current {
			delete(profilePending, platform)
			if len(profilePending) == 0 {
				delete(r.profileFailedPlatforms, profileName)
			}
		}
	}()
	statusKey := fmt.Sprintf("%s:%s", profileName, platform)
	for r.running.Load() {
		var adapter gateway.PlatformAdapter
		rearm := false
		stop, err := func() (stop bool, err error) {
			defer func() {
				if rec := recover(); rec != nil {
					err = fmt.Errorf("%v", rec)
				}
			}()
			built, success, err := r.secondaryReconnectAttempt(ctx, profileName, platform, predecessor)
			if err != nil {
				return false, err
			}
			adapter = built
			if adapter == nil {
				if r.endSecondaryReconnect(profileName, platform, predecessor, nil, success) {
					return true, nil
				}
				rearm = true
			} else if *success && r.running.Load() && r.installSecondaryReconnect(ctx, profileName, platform, predecessor, adapter) {
				return true, nil
			}
			if adapter != nil {
				r.safeAdapterDisconnect(ctx, adapter, platform)
				gateway.HandOverHeldInbound(adapter, predecessor)
				if r.endSecondaryReconnect(profileName, platform, predecessor, adapter, success) {
					return true, nil
				}
			}
			return false, nil
		}()
		if err != nil {
			if adapter != nil {
				r.safeAdapterDisconnect(ctx, adapter, platform)
				profileMap, _ := r.profileAdapters.Get(profileName)
				if live, _ := profileMap.Get(platform); live != adapter {
					gateway.HandOverHeldInbound(adapter, predecessor)
				}
			}
			if isCancellation(err) || ctx.Err() != nil {
				return err
			}
			runLog().Debug(fmt.Sprintf("Secondary %s reconnect attempt failed (profile: %s)", platform, profileName), "exc_info", err)
		}
		if stop {
			return nil
		}
		if !r.running.Load() {
			return nil
		}
		attempts++
		queueInfo["attempts"] = attempts
		scoped := scopeOrNull(ctx, profileScopeNoHydrate, routedProfileHome(profileName))
		if rearm {
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						logSuppressed(slog.LevelDebug, fmt.Errorf("%v", rec), "plugin load re-arm for %s failed (profile: %s): %s", platform, profileName)
					}
				}()
				logSuppressed(slog.LevelDebug, r.rearmFailedPluginLoad(scoped, platform, queueInfo), "plugin load re-arm for %s failed (profile: %s): %s", platform, profileName)
			}()
		}
		flagged := r.flagReconnectNeedsAttention(scoped, platform, queueInfo, pythread.Monotonic(), statusKey)
		if flagged && rearm {
			r.markAdapterUnavailable(scoped, statusKey, platform, true, nil)
		}
		backoff := reconnectBackoff(attempts)
		runLog().Info(fmt.Sprintf("Secondary %s reconnect retry in %ds (profile: %s)", platform, int(backoff), profileName))
		if err := sleepCtx(ctx, backoff); err != nil {
			return err
		}
	}
	return nil
}

// Parks a cold start reconnect until the runner is live, then hands it to the regular scheduler
func (r *GatewayRunner) scheduleSecondaryProfileStartupReconnect(ctx context.Context, profileName string, platform gateway.Platform, adapter gateway.PlatformAdapter) {
	if adapter != nil && !adapter.Base().FatalErrorRetryable() {
		return
	}
	if adapter != nil {
		base := adapter.Base()
		if code := base.FatalErrorCode(); code != nil && gateway.IsGlobalStartupConflict(*code) {
			runLog().Error(fmt.Sprintf("[MULTIPLEX] Profile '%s': %s credential is held by another "+
				"gateway (%s) — parked, not retried. %s", profileName, platform, *code, orText(base.FatalErrorMessage(), "")))
			r.markPlatformFatal(ctx, fmt.Sprintf("%s:%s", profileName, platform), adapter)
			return
		}
	}
	profileMap, _ := r.profileAdapters.Get(profileName)
	handoff := func(c context.Context) {
		if now, _ := r.profileAdapters.Get(profileName); now != profileMap {
			return
		}
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Error(fmt.Sprintf("secondary-startup-reconnect handoff failed (profile=%s platform=%s)", profileName, platform), "exc_info", rec)
			}
		}()
		r.scheduleSecondaryProfileReconnect(c, profileName, platform, adapter)
	}
	gate := make(chan struct{})
	bridge := r.retainBackgroundTask(spawnGatewayTask(context.WithoutCancel(ctx), fmt.Sprintf("secondary-startup-reconnect:%s:%s", profileName, platform), func(c context.Context) (any, error) {
		<-gate
		if r.running.Load() {
			handoff(c)
			return nil, nil
		}
		for !r.running.Load() && !r.shutdownEvent.IsSet() {
			if err := sleepCtx(c, 0.1); err != nil {
				return nil, err
			}
		}
		if r.running.Load() && !r.shutdownEvent.IsSet() {
			handoff(c)
		}
		return nil, nil
	}))
	if adapter != nil {
		r.holdSecondaryClaims(profileName, platform, bridge, r.adapterClaims(platform, adapter))
	}
	close(gate)
}

// Schedules one runner owned secondary reconnect, none for a retiring profile or one already pending
func (r *GatewayRunner) scheduleSecondaryProfileReconnect(ctx context.Context, profileName string, platform gateway.Platform, adapter gateway.PlatformAdapter) {
	if !r.running.Load() || (adapter != nil && !adapter.Base().FatalErrorRetryable()) {
		return
	}
	if _, retiring := r.retiringProfiles[profileName]; retiring {
		return
	}
	r.profileFailedMu.Lock()
	if r.profileFailedPlatforms == nil {
		r.profileFailedPlatforms = map[string]map[gateway.Platform]*gatewayTask{}
	}
	profilePending := r.profileFailedPlatforms[profileName]
	if profilePending == nil {
		profilePending = map[gateway.Platform]*gatewayTask{}
		r.profileFailedPlatforms[profileName] = profilePending
	}
	if _, pending := profilePending[platform]; pending {
		r.profileFailedMu.Unlock()
		return
	}
	gate := make(chan struct{})
	task := r.retainBackgroundTask(spawnGatewayTask(context.WithoutCancel(ctx), fmt.Sprintf("secondary-reconnect:%s:%s", profileName, platform), func(c context.Context) (any, error) {
		<-gate
		return nil, r.runSecondaryProfileReconnect(c, profileName, platform, adapter)
	}))
	profilePending[platform] = task
	r.profileFailedMu.Unlock()
	if adapter != nil {
		r.holdSecondaryClaims(profileName, platform, task, r.adapterClaims(platform, adapter))
	}
	close(gate)
}
