package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Message handler with the object it is bound to, nil owner for a plain function
type boundMessageHandler struct {
	owner any
	fn    gateway.MessageHandler
}

// Callbacks and policies of one adapter wiring, zero values taking the primary handlers and runner policies
type wireAdapterOptions struct {
	messageHandler       *boundMessageHandler
	fatalErrorHandler    gateway.FatalErrorHandler
	busySessionHandler   gateway.BusySessionHandler
	authorizationCheck   gateway.AuthorizationCheck
	platformEventHandler gateway.PlatformEventHandler
	busyTextMode         *string
	busyTextTiming       *[2]float64
	humanDelaySet        bool
	humanDelay           *[2]int
}

// Telegram adapter surface that moves an opted in root DM message into a fresh topic
type topicTransferAdapter interface {
	SetTopicTransferFn(fn func(ctx context.Context, source *gateway.SessionSource) (threadID string, created bool, ok bool, err error))
}

// Installs the runner callbacks every adapter needs, the primary handlers unless the options carry profile scoped ones
func (r *GatewayRunner) wireAdapterHandlers(ctx context.Context, adapter gateway.PlatformAdapter, o wireAdapterOptions) {
	base := adapter.Base()
	handler := o.messageHandler
	if handler == nil {
		handler = r.primaryMessageHandler(ctx)
	}
	base.SetBoundMessageHandler(handler.owner, handler.fn)
	fatal := o.fatalErrorHandler
	if fatal == nil {
		fatal = r.handleAdapterFatalError
	}
	base.SetFatalErrorHandler(fatal)
	var store any
	if r.sessionStore != nil {
		store = r.sessionStore
	}
	base.SetSessionStore(store)
	busy := o.busySessionHandler
	if busy == nil {
		busy = r.primaryBusySessionHandler(ctx)
	}
	base.SetBusySessionHandler(busy)
	base.SetReactionHandler(func(c context.Context, event map[string]any) error {
		r.handleReactionEvent(c, event)
		return nil
	})
	detached := context.WithoutCancel(ctx)
	base.SetTopicRecoveryFn(func(source *gateway.SessionSource) (*string, error) {
		return r.recoverTelegramTopicThreadID(detached, source), nil
	})
	if transfer, ok := adapter.(topicTransferAdapter); ok {
		transfer.SetTopicTransferFn(r.transferTelegramRootDM)
	}
	check := o.authorizationCheck
	if check == nil {
		check = r.makeAdapterAuthCheck(ctx, base.Platform, "")
	}
	base.SetAuthorizationCheck(check)
	events := o.platformEventHandler
	if events == nil {
		events = r.primaryPlatformEventHandler(ctx)
	}
	base.SetPlatformEventHandler(events)
	mode := r.busyTextMode
	if o.busyTextMode != nil {
		mode = *o.busyTextMode
	}
	base.SetBusyTextMode(mode)
	timing := o.busyTextTiming
	if timing == nil && r.busyTextTimingByProfile != nil {
		timing = &r.busyTextTiming
	}
	if timing != nil {
		base.SetBusyTextTiming(timing[0], timing[1])
	}
	if o.humanDelaySet {
		base.SetHumanDelayRangeMs(o.humanDelay)
	} else {
		base.SetHumanDelayRangeMs(r.humanDelay)
	}
}

// Installs the profile scoped handlers shared by a secondary adapter's startup and reconnect
func (r *GatewayRunner) configureProfileAdapter(ctx context.Context, adapter gateway.PlatformAdapter, profileName string, platform gateway.Platform) {
	base := adapter.Base()
	base.Traits.RuntimeStatusPlatformKey = fmt.Sprintf("%s:%s", profileName, platform)
	base.SetOwnerProfile(profileName)
	o := wireAdapterOptions{
		messageHandler:       r.makeProfileMessageHandler(ctx, profileName),
		fatalErrorHandler:    r.makeProfileFatalErrorHandler(profileName, platform),
		busySessionHandler:   r.makeProfileBusySessionHandler(ctx, profileName),
		authorizationCheck:   r.makeAdapterAuthCheck(ctx, platform, profileName),
		platformEventHandler: r.makeProfilePlatformEventHandler(ctx, profileName),
	}
	mode := r.busyTextMode
	if r.busyTextModesByProfile != nil {
		if m, ok := r.busyTextModesByProfile[profileName]; ok {
			mode = m
		}
	}
	o.busyTextMode = &mode
	if r.busyTextTimingByProfile != nil {
		if t, ok := r.busyTextTimingByProfile[profileName]; ok {
			o.busyTextTiming = &t
		}
	}
	if r.humanDelayByProfile != nil {
		if d, ok := r.humanDelayByProfile[profileName]; ok {
			o.humanDelaySet, o.humanDelay = true, d
		}
	}
	r.wireAdapterHandlers(ctx, adapter, o)
	r.bindVoiceInputCallback(ctx, adapter)
	base.SetProfileName(profileName)
	var extra map[string]any
	if base.Config != nil {
		extra = base.Config.Extra
	}
	if r.multiplexOn() && !gateway.SharedListenerMirrorPlatforms[string(platform)] && gateway.PlatformBindsPort(string(platform), extra) {
		base.SetSharedListenerProfile(profileName)
	}
}

// Fatal error handler routing a secondary profile's failure to that profile's reconnect slot
func (r *GatewayRunner) makeProfileFatalErrorHandler(profileName string, platform gateway.Platform) gateway.FatalErrorHandler {
	return func(ctx context.Context, adapter gateway.PlatformAdapter) error {
		return r.handleProfileAdapterFatalError(ctx, profileName, platform, adapter)
	}
}

// Removes a failed multiplexed adapter and schedules its profile scoped reconnect, ignoring a stale notification
func (r *GatewayRunner) handleProfileAdapterFatalError(ctx context.Context, profileName string, platform gateway.Platform, adapter gateway.PlatformAdapter) error {
	profileMap, _ := r.profileAdapters.Get(profileName)
	if current, ok := profileMap.Get(platform); profileMap == nil || !ok || current != adapter {
		runLog().Debug(fmt.Sprintf("Ignoring stale fatal error from secondary %s adapter (profile: %s)", platform, profileName))
		return nil
	}
	profileMap.Delete(platform)
	r.holdSecondaryClaims(profileName, platform, adapter, r.adapterClaims(platform, adapter))
	settled := func() bool {
		defer r.releaseSecondaryClaims(profileName, platform, adapter)
		if profileHome := routedProfileHome(profileName); profileHome != unresolvedProfileHome {
			observability.RecordPlatformDisconnect(ctx, adapter, profileHome.path)
		}
		r.safeAdapterDisconnect(ctx, adapter, platform)
		if now, _ := r.profileAdapters.Get(profileName); !r.running.Load() || now != profileMap {
			return false
		}
		r.scheduleSecondaryProfileReconnect(ctx, profileName, platform, adapter)
		return true
	}()
	if !settled {
		return nil
	}
	code := "unknown"
	if c := adapter.Base().FatalErrorCode(); c != nil && *c != "" {
		code = *c
	}
	runLog().Error(fmt.Sprintf("Fatal %s adapter error for multiplexed profile %s (%s)", platform, profileName, code))
	return nil
}

// Runtime scope of an async body, the hydrated profile scope for a home, launch scope for none, unchanged when unresolved
func asyncScopeOrNull(ctx context.Context, home *routedHome) context.Context {
	return scopeOrNull(ctx, func(c context.Context, h string) context.Context {
		return gateway.ProfileRuntimeScope(c, h, true)
	}, home)
}

// Message handler that canonicalizes the event identity first and then runs it under the profile scope
func (r *GatewayRunner) makeProfileMessageHandler(ctx context.Context, profileName string) *boundMessageHandler {
	profileHome := routedProfileHome(profileName)
	var transportHome *string
	if profileHome != unresolvedProfileHome {
		transportHome = &profileHome.path
	}
	profile := profileName
	if err := gateway.RegisterTransportHome(r, &profile, transportHome); err != nil {
		runLog().Debug(fmt.Sprintf("transport home registration for '%s' failed", profileName), "error", err)
	}
	return &boundMessageHandler{owner: r, fn: func(c context.Context, event *gateway.MessageEvent) (any, error) {
		identity := r.canonicalize(c, event.Source, profileName, "")
		if transportHome != nil {
			gateway.SetAuthorizationProfileHome(event.Source, *transportHome)
		}
		c, err := gateway.NativeCallback(c, r, event, transportHome, &profile)
		if err != nil {
			return nil, err
		}
		return r.handleMessage(asyncScopeOrNull(c, routedRuntimeHome(identity, profileHome)), event)
	}}
}

// Busy path twin of the profile message handler, resolving busy policy under the profile scope
func (r *GatewayRunner) makeProfileBusySessionHandler(ctx context.Context, profileName string) gateway.BusySessionHandler {
	profileHome := routedProfileHome(profileName)
	return func(c context.Context, event *gateway.MessageEvent, _ string) (bool, error) {
		identity := r.canonicalize(c, event.Source, profileName, "")
		c = asyncScopeOrNull(c, routedRuntimeHome(identity, profileHome))
		key, err := r.sessionKeyForSource(c, event.Source)
		if err != nil {
			return false, err
		}
		return r.handleActiveSessionBusyMessage(c, event, key)
	}
}

// Message handler scoping primary adapter messages to their routed multiplex profile
func (r *GatewayRunner) makeDefaultProfileMessageHandler(ctx context.Context) *boundMessageHandler {
	defaultHome := home.Dir(ctx)
	if err := gateway.RegisterTransportHome(r, nil, &defaultHome); err != nil {
		runLog().Debug("transport home registration for the default profile failed", "error", err)
	}
	return &boundMessageHandler{owner: r, fn: func(c context.Context, event *gateway.MessageEvent) (any, error) {
		profileHome, admitted := r.admitPrimarySource(c, event.Source, defaultHome)
		if !admitted {
			profileHome = defaultHome
		}
		c, err := gateway.NativeCallback(c, r, event, &defaultHome, nil)
		if err != nil {
			return nil, err
		}
		return r.handleMessage(gateway.ProfileRuntimeScope(c, profileHome, true), event)
	}}
}

// Busy path twin of the default profile message handler, swallowing a rejected route
func (r *GatewayRunner) makeDefaultProfileBusySessionHandler(ctx context.Context) gateway.BusySessionHandler {
	defaultHome := home.Dir(ctx)
	return func(c context.Context, event *gateway.MessageEvent, _ string) (bool, error) {
		profileHome, admitted := r.admitPrimarySource(c, event.Source, defaultHome)
		if !admitted {
			return true, nil
		}
		c = gateway.ProfileRuntimeScope(c, profileHome, true)
		key, err := r.sessionKeyForSource(c, event.Source)
		if err != nil {
			return false, err
		}
		return r.handleActiveSessionBusyMessage(c, event, key)
	}
}

// Scoped message handler of a primary adapter, bound to the runner under multiplexing or a shared authority
func (r *GatewayRunner) primaryMessageHandler(ctx context.Context) *boundMessageHandler {
	if r.multiplexOn() || r.SessionAuthority() != nil {
		return r.makeDefaultProfileMessageHandler(ctx)
	}
	return &boundMessageHandler{fn: func(c context.Context, event *gateway.MessageEvent) (any, error) {
		return r.handleMessage(standaloneScoped(c), event)
	}}
}

// Scoped busy session handler of a primary adapter
func (r *GatewayRunner) primaryBusySessionHandler(ctx context.Context) gateway.BusySessionHandler {
	if r.multiplexOn() {
		return r.makeDefaultProfileBusySessionHandler(ctx)
	}
	return func(c context.Context, event *gateway.MessageEvent, sessionKey string) (bool, error) {
		return r.handleActiveSessionBusyMessage(standaloneScoped(c), event, sessionKey)
	}
}

// Standalone handler scope, the launch credential body marked and the launch profile bound once another profile is hosted
func standaloneScoped(ctx context.Context) context.Context {
	return standaloneLaunchScope(provider.WithLaunchBody(ctx))
}

// Authorizes one normalized adapter event and publishes it to the platform event hook, never raising
func (r *GatewayRunner) handleGatewayPlatformEvent(ctx context.Context, event map[string]any, source *gateway.SessionSource) error {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug("gateway_platform_event hook dispatch failed", "error", rec)
		}
	}()
	if !agent.LifecycleHasHook(ctx, "gateway_platform_event") {
		return nil
	}
	authorized, err := r.isUserAuthorizedForSource(ctx, source, false)
	if err != nil {
		runLog().Debug("gateway_platform_event hook dispatch failed", "error", err)
		return nil
	}
	if authorized {
		agent.LifecycleInvokeHook(ctx, "gateway_platform_event", event)
	}
	return nil
}

// Platform event handler bound to one multiplex profile's authorization and hook dispatch
func (r *GatewayRunner) makeProfilePlatformEventHandler(ctx context.Context, profileName string) gateway.PlatformEventHandler {
	profileHome := routedProfileHome(profileName)
	return func(c context.Context, event map[string]any, source *gateway.SessionSource) error {
		r.canonicalize(c, source, profileName, "")
		return r.handleGatewayPlatformEvent(scopeOrNull(c, func(s context.Context, h string) context.Context {
			return gateway.ProfileRuntimeScope(s, h, true)
		}, profileHome), event, source)
	}
}

// Platform event handler scoping primary transport events to their routed multiplex profile
func (r *GatewayRunner) makeDefaultProfilePlatformEventHandler(ctx context.Context) gateway.PlatformEventHandler {
	defaultHome := home.Dir(ctx)
	return func(c context.Context, event map[string]any, source *gateway.SessionSource) error {
		profileHome, admitted := r.admitPrimarySource(c, source, defaultHome)
		if !admitted {
			return nil
		}
		return r.handleGatewayPlatformEvent(gateway.ProfileRuntimeScope(c, profileHome, true), event, source)
	}
}

// Scoped platform event handler of a primary adapter
func (r *GatewayRunner) primaryPlatformEventHandler(ctx context.Context) gateway.PlatformEventHandler {
	if r.multiplexOn() {
		return r.makeDefaultProfilePlatformEventHandler(ctx)
	}
	return func(c context.Context, event map[string]any, source *gateway.SessionSource) error {
		return r.handleGatewayPlatformEvent(standaloneScoped(c), event, source)
	}
}
