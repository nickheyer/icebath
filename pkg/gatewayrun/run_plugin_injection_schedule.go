package gatewayrun

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
)

// Publishes this live gateway's plugin message scheduler process wide
func (r *GatewayRunner) installPluginMessageInjector() {
	plugins.PublishGatewayMessageHost(r, r.pluginMessageInjector)
}

// Scheduler the plugin host calls, a scheduling exception raised into the host guard the way Python raises it
func (r *GatewayRunner) pluginMessageInjector(ctx context.Context, kwargs map[string]any) bool {
	accepted, err := r.schedulePluginMessageInjection(ctx, kwargs)
	if err != nil {
		panic(err)
	}
	return accepted
}

// Keyword arguments of one plugin injection request, an empty plugin home standing for None
type pluginInjectionArgs struct {
	content           string
	pluginID          string
	sessionKey        *string
	origin            map[string]any
	pluginHome        string
	keyless           bool
	callingSessionKey string
}

// Request read from the injector keyword arguments, a TypeError for a missing required one
func pluginInjectionArgsFrom(kwargs map[string]any) (pluginInjectionArgs, error) {
	var args pluginInjectionArgs
	for _, name := range []string{"content", "plugin_id"} {
		if _, ok := kwargs[name]; !ok {
			return args, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("_schedule_plugin_message_injection() missing 1 required keyword-only argument: '%s'", name)}
		}
	}
	content, ok := kwargs["content"].(string)
	if !ok {
		return args, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("content must be str, not %s", pyrepr.TypeName(kwargs["content"]))}
	}
	pluginID, ok := kwargs["plugin_id"].(string)
	if !ok {
		return args, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("plugin_id must be str, not %s", pyrepr.TypeName(kwargs["plugin_id"]))}
	}
	args.content, args.pluginID = content, pluginID
	if key, isText := kwargs["session_key"].(string); isText {
		args.sessionKey = &key
	}
	if raw, present := kwargs["origin"]; present && raw != nil {
		origin, isMap := raw.(map[string]any)
		if !isMap {
			return args, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object is not a mapping", pyrepr.TypeName(raw))}
		}
		args.origin = origin
	}
	args.pluginHome, _ = kwargs["plugin_home"].(string)
	args.keyless = pyrepr.Truthy(kwargs["keyless"])
	args.callingSessionKey, _ = kwargs["calling_session_key"].(string)
	return args, nil
}

// Whether ctx runs as a task of the gateway loop rather than on an executor or plugin thread
func onGatewayLoop(ctx context.Context) bool {
	return currentGatewayTask(ctx) != nil && currentExecutorThread(ctx) == nil
}

// Whether the loop was closed so scheduling onto it fails
func loopIsClosed(loop gateway.EventLoop) bool {
	closer, ok := loop.(interface{ IsClosed() bool })
	return ok && closer.IsClosed()
}

// Schedules a plugin triggered turn on the live gateway loop, refusing up front what the plugin may not start
func (r *GatewayRunner) schedulePluginMessageInjection(ctx context.Context, kwargs map[string]any) (bool, error) {
	args, err := pluginInjectionArgsFrom(kwargs)
	if err != nil {
		return false, err
	}
	loop := r.gatewayLoop
	if !r.running.Load() || loop == nil || loopIsClosed(loop) {
		return false, nil
	}
	var target string
	var dispatch func(ctx context.Context) (bool, error)
	switch {
	case args.keyless:
		authority, live, err := keylessResolve(ctx, r, args.pluginHome, args.callingSessionKey, args.pluginID)
		if err != nil {
			return false, err
		}
		if live == nil {
			return false, nil
		}
		target = fmt.Sprintf("session=%s (no key given)", live.Route)
		dispatch = func(c context.Context) (bool, error) {
			return keylessAdmit(c, r, authority, live, args.content, args.pluginID)
		}
	case args.origin != nil:
		source, err := r.pluginOriginSource(ctx, args.origin, args.pluginHome, args.pluginID)
		if err != nil {
			return false, err
		}
		if source == nil {
			return false, nil
		}
		admissible, err := r.pluginOriginAdmissible(ctx, source, args.pluginHome, args.pluginID)
		if err != nil || !admissible {
			return false, err
		}
		target = fmt.Sprintf("origin=%s/%s", source.Platform, source.ChatID)
		dispatch = func(c context.Context) (bool, error) {
			return r.dispatchPluginOriginInjection(c, source, args.content, args.pluginID)
		}
	default:
		admissible, err := r.pluginSessionKeyAdmissible(ctx, args.sessionKey, args.pluginHome, args.pluginID)
		if err != nil || !admissible {
			return false, err
		}
		target = "session=" + strOrNone(args.sessionKey)
		dispatch = func(c context.Context) (bool, error) {
			return r.dispatchPluginMessageInjection(c, args.sessionKey, args.content, args.pluginID, args.pluginHome)
		}
	}
	run := func(c context.Context) (any, error) { return dispatch(c) }
	logResult := func(done *gatewayFuture) {
		if done.cancelled() {
			return
		}
		result, err := done.wait(context.Background())
		if err != nil {
			if !errors.Is(err, errTaskCancelled) {
				runLog().Warn(fmt.Sprintf("Plugin message injection failed: plugin=%s %s", args.pluginID, target), "exc_info", err)
			}
			return
		}
		if accepted, _ := result.(bool); accepted {
			return
		}
		runLog().Warn(fmt.Sprintf("Plugin message injection was not routed: plugin=%s %s", args.pluginID, target))
	}
	taskCtx := context.WithoutCancel(ctx)
	if onGatewayLoop(ctx) {
		r.retainBackgroundTask(spawnGatewayTask(taskCtx, "plugin-message-injection", run)).addDoneCallback(logResult)
		return true, nil
	}
	if err := loop.CallSoonThreadsafe(func() {
		spawnGatewayTask(taskCtx, "plugin-message-injection", run).addDoneCallback(logResult)
	}); err != nil {
		runLog().Warn(fmt.Sprintf("Plugin message injection scheduling failed: %s", err))
		return false, nil
	}
	return true, nil
}

// Text of a plugin home with None for an absent one
func homeOrNone(profileHome string) string {
	if profileHome == "" {
		return "None"
	}
	return profileHome
}

// Whether plugin turns are accepted, false once the runner stopped or started draining
func (r *GatewayRunner) pluginInjectionAccepting() bool {
	return r.running.Load() && !r.draining.Load()
}

// Profile runtime scope an injected turn runs under, the known owner home first, ctx unchanged without multiplexing
func (r *GatewayRunner) pluginInjectionScope(ctx context.Context, source *gateway.SessionSource, profileHome string) (context.Context, error) {
	if !r.multiplexOn() {
		return ctx, nil
	}
	if profileHome == "" {
		resolved, err := r.resolveProfileHomeForSource(ctx, source)
		if err != nil {
			return nil, err
		}
		profileHome = resolved
	}
	return gateway.ProfileRuntimeScope(ctx, profileHome, true), nil
}

// Current gateway authorization for the route an injected turn runs as, adapter time grants not counted
func (r *GatewayRunner) pluginInjectionAuthorized(ctx context.Context, source *gateway.SessionSource, pluginID, target string) bool {
	authorized, err := func() (ok bool, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		return r.isUserAuthorizedForSource(ctx, source, false)
	}()
	if err != nil {
		runLog().Warn(fmt.Sprintf("Plugin message injection authorization check failed: plugin=%s %s", pluginID, target), "exc_info", err)
		return false
	}
	if !authorized {
		runLog().Warn(fmt.Sprintf("Plugin message injection denied by current gateway authorization: plugin=%s %s", pluginID, target))
	}
	return authorized
}

// Refuses a keyed injection into another profile's session up front, an unknown key decided at dispatch
func (r *GatewayRunner) pluginSessionKeyAdmissible(ctx context.Context, sessionKey *string, pluginHome, pluginID string) (bool, error) {
	key := strOrEmpty(sessionKey)
	entry := r.sessionStore.LookupBySessionKey(ctx, key)
	if entry == nil || entry.Origin == nil {
		return true, nil
	}
	restored, err := r.restoredSource(ctx, entry)
	if err != nil {
		return false, err
	}
	source := gateway.ReplaceSource(restored, nil)
	if source.Platform == gateway.PlatformLocal {
		live, err := r.pluginLocalSession(ctx, source, key, pluginHome, pluginID)
		return live != nil, err
	}
	return r.pluginMessagingSessionOwned(ctx, key, pluginHome, pluginID), nil
}

// Text of an optional string, empty for an absent one
func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Whether a messaging session belongs to the plugin's own profile, a caller without a plugin home left to dispatch
func (r *GatewayRunner) pluginMessagingSessionOwned(ctx context.Context, sessionKey, pluginHome, pluginID string) bool {
	if pluginHome == "" {
		return true
	}
	profile := r.servedProfileForHome(ctx, pluginHome)
	owner := profile
	if profile != nil && r.multiplexOn() {
		owner = nil
		if keyed, ok := state.ProfileFromSessionKey(sessionKey); ok {
			owner = &keyed
		}
	}
	if profile != nil && owner != nil && *owner == *profile {
		return true
	}
	runLog().Warn(fmt.Sprintf("Plugin message injection refused: plugin=%s session=%s belongs to profile %s, not the plugin's profile %s",
		pluginID, sessionKey, strOrNone(owner), strOrNone(profile)))
	return false
}

// Live local session the plugin profile's own authority holds under the key, nil logged when it holds none
func (r *GatewayRunner) pluginLocalSession(ctx context.Context, source *gateway.SessionSource, sessionKey, pluginHome, pluginID string) (*gateway.LiveSession, error) {
	var authority *gateway.SessionAuthority
	if pluginHome != "" {
		found, err := gateway.AuthorityForHome(r, pluginHome)
		if err != nil {
			return nil, err
		}
		authority = found
	}
	var live *gateway.LiveSession
	if authority != nil {
		live, _ = authority.Live(source.ChatID)
	}
	if live != nil && live.Route == sessionKey {
		return live, nil
	}
	runLog().Warn(fmt.Sprintf("Plugin message injection refused: plugin=%s session=%s is not a live local session of the plugin's profile home %s",
		pluginID, sessionKey, homeOrNone(pluginHome)))
	return nil, nil
}

// Name of the profile this gateway serves at the plugin home, nil when it serves none there
func (r *GatewayRunner) servedProfileForHome(ctx context.Context, profileHome string) *string {
	if profileHome == "" {
		return nil
	}
	key := home.Key(profileHome)
	if r.multiplexOn() {
		served, err := func() (served []profiles.ServedProfile, err error) {
			defer func() {
				if rec := recover(); rec != nil {
					err = fmt.Errorf("%v", rec)
				}
			}()
			return gateway.MultiplexProfileHomes(ctx, r.config)
		}()
		if err != nil {
			runLog().Warn("Plugin message injection: served-profile set unavailable", "exc_info", err)
			return nil
		}
		for _, p := range served {
			if home.Key(p.Home) == key {
				name := p.Name
				return &name
			}
		}
		return nil
	}
	if home.Key(home.RoutingProcess()) != key {
		return nil
	}
	name := r.primaryProfileName
	if name == "" {
		name = r.activeProfileName(ctx)
	}
	return &name
}

// Canonical source of a plugin supplied origin, nil logged when the plugin may not start a turn there
func (r *GatewayRunner) pluginOriginSource(ctx context.Context, origin map[string]any, pluginHome, pluginID string) (*gateway.SessionSource, error) {
	refuse := func(reason string) {
		runLog().Warn(fmt.Sprintf("Plugin message injection refused: plugin=%s %s", pluginID, reason))
	}
	profile := r.servedProfileForHome(ctx, pluginHome)
	if profile == nil {
		refuse(fmt.Sprintf("its profile home %s is not served by this gateway", homeOrNone(pluginHome)))
		return nil, nil
	}
	fields := make(map[string]any, len(origin))
	for k, v := range origin {
		fields[k] = v
	}
	requested := fields["profile"]
	delete(fields, "profile")
	if named, isText := requested.(string); pyrepr.Truthy(requested) && (!isText || named != *profile) {
		refuse(fmt.Sprintf("origin names profile %s; plugins may only start sessions in their own profile (%s)", pyrepr.Repr(requested), *profile))
		return nil, nil
	}
	source, err := gateway.SessionSourceFromDict(ctx, fields)
	if err != nil {
		switch gateway.PyErrorKind(err) {
		case "KeyError", "TypeError", "ValueError":
			refuse(fmt.Sprintf("invalid origin (%s)", err))
			return nil, nil
		}
		return nil, err
	}
	where := fmt.Sprintf("%s/%s", source.Platform, source.ChatID)
	adapter, _ := r.adaptersForProfile(ctx, profile).Get(source.Platform)
	if adapter == nil {
		refuse(fmt.Sprintf("no connected %s adapter for profile %s", source.Platform, *profile))
		return nil, nil
	}
	source.SetTransportAdapterRef(adapter)
	identity := r.canonicalize(ctx, source, "", "")
	if identity == nil {
		refuse(fmt.Sprintf("origin %s does not resolve to a served profile", where))
		return nil, nil
	}
	if identity.RuntimeProfile != *profile {
		refuse(fmt.Sprintf("origin %s routes to profile %s, not the plugin's profile %s", where, identity.RuntimeProfile, *profile))
		return nil, nil
	}
	return source, nil
}

// Whether an origin holds a committed human turn the plugin turn can borrow, true when no authority serves the plugin
func (r *GatewayRunner) pluginOriginAdmissible(ctx context.Context, source *gateway.SessionSource, pluginHome, pluginID string) (bool, error) {
	var authority *gateway.SessionAuthority
	if pluginHome != "" {
		found, err := gateway.AuthorityForHome(r, pluginHome)
		if err != nil {
			return false, err
		}
		authority = found
	}
	if authority == nil {
		return true, nil
	}
	sessionKey, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return false, err
	}
	if entry := r.sessionStore.LookupBySessionKey(ctx, sessionKey); entry != nil {
		envelope, err := gateway.CommittedRouteEnvelope(ctx, authority, entry)
		if err != nil {
			return false, err
		}
		if envelope != nil {
			return true, nil
		}
	}
	runLog().Warn(fmt.Sprintf("Plugin message injection refused: plugin=%s origin=%s/%s has no human message admitted yet; a plugin can continue a chat, not open one",
		pluginID, source.Platform, source.ChatID))
	return false, nil
}
