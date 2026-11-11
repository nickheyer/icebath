package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Home of a named routed profile, the unresolved marker never standing for the launch profile
type routedHome struct {
	path       string
	unresolved bool
}

// Marker of a named routed profile whose home does not resolve
var unresolvedProfileHome = &routedHome{unresolved: true}

// Waits up to timeout for the task, detaching it on deadline or on the caller's cancellation, true when it finished
func waitOrDetach(ctx context.Context, task *gatewayTask, timeout float64) (bool, error) {
	timer := time.NewTimer(time.Duration(timeout * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-task.done:
		return true, nil
	case <-timer.C:
		task.Cancel()
		return false, nil
	case <-ctx.Done():
		task.Cancel()
		return false, ctx.Err()
	}
}

// Runs an adapter cleanup bounded by a detach on deadline, true when it completed
func (r *GatewayRunner) awaitAdapterCleanupWithTimeout(ctx context.Context, cleanup func(ctx context.Context) error, timeout float64) (bool, error) {
	if timeout <= 0 {
		return true, cleanup(ctx)
	}
	task := spawnGatewayTask(ctx, "adapter-cleanup", func(taskCtx context.Context) (any, error) {
		return nil, cleanup(taskCtx)
	})
	finished, err := waitOrDetach(ctx, task, timeout)
	if err != nil || !finished {
		return false, err
	}
	_, err = task.wait(ctx)
	return true, err
}

// Disconnects an adapter defensively after a failed connect, bounded and never raising
func (r *GatewayRunner) safeAdapterDisconnect(ctx context.Context, adapter gateway.PlatformAdapter, platform gateway.Platform) {
	timeout := r.adapterDisconnectTimeoutSecs()
	label := string(platform)
	if label == "" {
		label = "adapter"
	}
	finished, err := r.awaitAdapterCleanupWithTimeout(ctx, adapter.Disconnect, timeout)
	if err != nil {
		if ctx.Err() == nil {
			logSuppressed(slog.LevelDebug, err, "Defensive %s disconnect after failed connect raised: %v", label)
		}
		return
	}
	if !finished {
		runLog().Warn(fmt.Sprintf("Timed out after %.1fs while disconnecting %s adapter; continuing shutdown", timeout, label))
	}
}

// Tears down one adapter on the shutdown path with bounded waits, never raising
func (r *GatewayRunner) boundedAdapterTeardown(ctx context.Context, adapter gateway.PlatformAdapter, platform gateway.Platform, profile string) {
	timeout := r.adapterDisconnectTimeoutSecs()
	suffix := ""
	if profile != "" {
		suffix = fmt.Sprintf(" (profile: %s)", profile)
	}
	startedAt := time.Now()
	finished, err := r.awaitAdapterCleanupWithTimeout(ctx, func(taskCtx context.Context) error {
		adapter.CancelBackgroundTasks(taskCtx)
		return nil
	}, timeout)
	if err != nil {
		runLog().Debug(fmt.Sprintf("✗ %s background-task cancel error%s: %v", platform, suffix, err))
	} else if !finished {
		runLog().Warn(fmt.Sprintf("✗ %s background-task cancel timed out after %.1fs - forcing continue%s", platform, timeout, suffix))
	}
	elapsed := time.Since(startedAt).Seconds()
	finished, err = r.awaitAdapterCleanupWithTimeout(ctx, adapter.Disconnect, timeout)
	if err != nil {
		logSuppressed(slog.LevelError, err, "✗ %s disconnect error after %.2fs%s: %v", platform, elapsed, suffix)
		return
	}
	if finished {
		runLog().Info(fmt.Sprintf("✓ %s disconnected (%.2fs)%s", platform, time.Since(startedAt).Seconds(), suffix))
		return
	}
	runLog().Warn(fmt.Sprintf("✗ %s disconnect timed out after %.1fs - forcing continue%s", platform, timeout, suffix))
}

// Non negative float of an environment variable, nil when unset or unparseable with a warning
func envTimeoutOverride(name string) *float64 {
	raw := pystr.Strip(os.Getenv(name))
	if raw == "" {
		return nil
	}
	value, err := pyint.ParseFloat(raw)
	if err != nil {
		runLog().Warn(fmt.Sprintf("Ignoring invalid %s=%s", name, pyrepr.Repr(raw)))
		return nil
	}
	if !(value > 0) {
		value = 0
	}
	return &value
}

// Per adapter disconnect timeout of the shutdown path
func (r *GatewayRunner) adapterDisconnectTimeoutSecs() float64 {
	if override := envTimeoutOverride("ICEBATH_GATEWAY_ADAPTER_DISCONNECT_TIMEOUT"); override != nil {
		return *override
	}
	return adapterDisconnectTimeoutSecsDefault
}

// Per platform connect timeout, the capped Telegram budget on the cold start connect
func (r *GatewayRunner) platformConnectTimeoutSecs(platform gateway.Platform, initial bool) float64 {
	if override := envTimeoutOverride("ICEBATH_GATEWAY_PLATFORM_CONNECT_TIMEOUT"); override != nil {
		return *override
	}
	if platform != gateway.PlatformTelegram {
		return platformConnectTimeoutSecsDefault
	}
	if initial {
		return telegramInitialConnectTimeoutSecsDefault
	}
	return telegramConnectTimeoutSecsDefault
}

// Connects an adapter within its platform bound and records the outcome
func (r *GatewayRunner) connectAdapterWithTimeout(ctx context.Context, adapter gateway.PlatformAdapter, platform gateway.Platform, isReconnect, initial bool) (bool, error) {
	ok, err := r.connectAdapterBounded(ctx, adapter, platform, isReconnect, initial)
	if err != nil {
		observability.RecordPlatformConnect(ctx, adapter, string(platform), isReconnect, false, err)
		return false, err
	}
	observability.RecordPlatformConnect(ctx, adapter, string(platform), isReconnect, ok, nil)
	return ok, nil
}

// Connect bounded by the platform timeout, a TimeoutError once it passes
func (r *GatewayRunner) connectAdapterBounded(ctx context.Context, adapter gateway.PlatformAdapter, platform gateway.Platform, isReconnect, initial bool) (bool, error) {
	timeout := r.platformConnectTimeoutSecs(platform, initial)
	if timeout <= 0 {
		return adapter.Connect(ctx, isReconnect)
	}
	task := spawnGatewayTask(ctx, "adapter-connect", func(taskCtx context.Context) (any, error) {
		return adapter.Connect(taskCtx, isReconnect)
	})
	finished, err := waitOrDetach(ctx, task, timeout)
	if err != nil {
		return false, err
	}
	if finished {
		result, err := task.wait(ctx)
		if err != nil {
			return false, err
		}
		ok, _ := result.(bool)
		return ok, nil
	}
	return false, &gateway.PyError{Kind: "TimeoutError", Msg: fmt.Sprintf("%s connect timed out after %s", platform, pyFormatG(timeout))}
}

// Python format of a float under the g presentation
func pyFormatG(value float64) string {
	text, err := pyrepr.Format(value, "g")
	if err != nil {
		return pyrepr.Str(value)
	}
	return text
}

// Cold start connect with the replace intent visible only during this wait
func (r *GatewayRunner) connectInitialAdapterWithTimeout(ctx context.Context, adapter gateway.PlatformAdapter, platform gateway.Platform) (bool, error) {
	adapter.Base().SetPlatformLockTakeoverAllowed(r.platformLockTakeoverOnStart)
	defer adapter.Base().SetPlatformLockTakeoverAllowed(false)
	return r.connectAdapterWithTimeout(ctx, adapter, platform, false, true)
}

// Home of a named routed profile, the unresolved marker when it does not resolve
func routedProfileHome(profileName string) *routedHome {
	dir, err := profiles.GetProfileDir(profileName)
	if err != nil {
		runLog().Warn(fmt.Sprintf("Profile home for '%s' does not resolve; its handlers run with no profile scope "+
			"(credential reads fail closed instead of borrowing the launch profile's)", profileName), "error", err)
		return unresolvedProfileHome
	}
	return &routedHome{path: dir}
}

// Runtime scope of one body, the profile scope for a home, the launch scope for none and nothing for an unresolved home
func scopeOrNull(ctx context.Context, scopeFactory func(ctx context.Context, home string) context.Context, home *routedHome) context.Context {
	if home == unresolvedProfileHome {
		return ctx
	}
	if home != nil {
		return scopeFactory(ctx, home.path)
	}
	return provider.WithLaunchProfileScopeIfMultiplexed(ctx)
}

// Pinned routing identity of the source, resolving it once, nil for a rejected or unresolvable route
func (r *GatewayRunner) canonicalize(ctx context.Context, source *gateway.SessionSource, transportProfile, primaryHome string) *gateway.RoutingIdentity {
	if source == nil {
		return nil
	}
	identity, err := gateway.CanonicalIdentity(ctx, source, r, nil, transportProfile, primaryHome)
	if err != nil {
		runLog().Debug("identity resolution failed; legacy readers stay in force", "error", err)
		return nil
	}
	if transportProfile != "" && (source.Profile == nil || *source.Profile == "") {
		profile := transportProfile
		source.Profile = &profile
	}
	return identity
}

// Runtime home of a secondary bot's routed event, the transport home when no identity resolved
func routedRuntimeHome(identity *gateway.RoutingIdentity, home *routedHome) *routedHome {
	if identity == nil || home == unresolvedProfileHome {
		return home
	}
	return &routedHome{path: identity.RuntimeHome}
}

// Runtime home a primary adapter source runs under, false when its route targets an unserved profile
func (r *GatewayRunner) admitPrimarySource(ctx context.Context, source *gateway.SessionSource, defaultHome string) (string, bool) {
	if identity := r.canonicalize(ctx, source, "", defaultHome); identity != nil {
		return identity.RuntimeHome, true
	}
	if source.ProfileRouteRejected {
		return "", false
	}
	return defaultHome, true
}

// Whether the runner config enables multiplexed profiles
func (r *GatewayRunner) multiplexOn() bool {
	return r.config != nil && r.config.MultiplexProfiles != nil && *r.config.MultiplexProfiles
}

// Registered flag and owning secondary profile of a live adapter, empty for the primary
func (r *GatewayRunner) OwningProfile(adapter gateway.PlatformAdapter, platform gateway.Platform) (bool, string) {
	registered, profile := r.owningProfile(adapter, platform)
	if profile == nil {
		return registered, ""
	}
	return registered, *profile
}

// Runtime home of a routed source with its missing profile fallback
func (r *GatewayRunner) ResolveProfileHomeForSource(ctx context.Context, source *gateway.SessionSource) (string, error) {
	return r.resolveProfileHomeForSource(ctx, source)
}

// Runner side identity seam of producer ingress
func (r *GatewayRunner) Canonicalize(ctx context.Context, source *gateway.SessionSource, transportProfile, primaryHome string) *gateway.RoutingIdentity {
	return r.canonicalize(ctx, source, transportProfile, primaryHome)
}

// Runtime home of a primary adapter source, false for a route to an unserved profile
func (r *GatewayRunner) AdmitPrimarySource(ctx context.Context, source *gateway.SessionSource, defaultHome string) (string, bool) {
	return r.admitPrimarySource(ctx, source, defaultHome)
}

// Launch session authority bound by its initialization, nil before it is
func (r *GatewayRunner) SessionAuthority() *gateway.SessionAuthority {
	r.sessionAuthorityMu.Lock()
	defer r.sessionAuthorityMu.Unlock()
	return r.sessionAuthority
}

// Binds the launch session authority
func (r *GatewayRunner) SetSessionAuthority(a *gateway.SessionAuthority) {
	r.sessionAuthorityMu.Lock()
	r.sessionAuthority = a
	r.sessionAuthorityMu.Unlock()
}

var _ gateway.ProducerScopeRunner = (*GatewayRunner)(nil)
