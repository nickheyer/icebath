package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Canonical source of a synthetic background process event, the persisted origin preferred
func (r *GatewayRunner) buildProcessEventSource(ctx context.Context, evt map[string]any) (*gateway.SessionSource, error) {
	sessionKey := pystr.Strip(truthyText(evt["session_key"]))
	derived := map[string]any{}
	if sessionKey != "" {
		if entry := r.sessionStore.LoadedEntry(ctx, sessionKey); entry != nil && entry.Origin != nil {
			source, err := r.restoredSource(ctx, entry)
			if err == nil {
				return source, nil
			}
			inboundLog().Debug(fmt.Sprintf("Synthetic process-event session-store lookup failed for %s: %s", sessionKey, err))
		}
		cached, err := gateway.DetachedLiveSource(r, r.sessionStore, sessionKey)
		if err != nil {
			return nil, err
		}
		if cached == nil {
			cached = r.getCachedSessionSource(sessionKey)
		}
		if cached != nil {
			return cached, nil
		}
		if parsed := gateway.ParseSessionKey(sessionKey); parsed != nil {
			derived = parsed
		}
	}
	pick := func(field string) string {
		if v := truthyText(evt[field]); v != "" {
			return v
		}
		return truthyText(derived[field])
	}
	platformName := pystr.Lower(pystr.Strip(pick("platform")))
	chatType := pystr.Lower(pystr.Strip(pick("chat_type")))
	chatID := pystr.Strip(pick("chat_id"))
	if platformName == "" || chatType == "" || chatID == "" {
		if rawProcessEventSessionID(evt) != "" {
			return nil, nil
		}
		evtType, ok := evt["type"]
		if !ok {
			evtType = "?"
		}
		inboundLog().Warn(fmt.Sprintf("Synthetic event source unresolvable: session_key=%s platform=%s chat_type=%s chat_id=%s evt_type=%s",
			pyrepr.Repr(sessionKey), pyrepr.Repr(platformName), pyrepr.Repr(chatType), pyrepr.Repr(chatID), pyrepr.Str(evtType)))
		return nil, nil
	}
	platform, err := gateway.ParsePlatform(ctx, platformName)
	if err == nil && !gateway.BuiltinPlatformValues[string(platform)] && !gateway.Registry.IsRegistered(ctx, string(platform)) {
		err = errors.New(platformName)
	}
	if err != nil {
		inboundLog().Warn(fmt.Sprintf("Synthetic process event has invalid platform metadata: %s", pyrepr.Repr(platformName)))
		return nil, nil
	}
	opt := func(field string) *string {
		if v := pystr.Strip(truthyText(evt[field])); v != "" {
			return &v
		}
		return nil
	}
	scopeID := opt("scope_id")
	if scopeID == nil && chatType != "dm" && chatType != "thread" {
		inboundLog().Warn(fmt.Sprintf("Synthetic event source for %s chat=%s (%s) reconstructed without scope_id; scoped relay egress may be declined by the connector's tenant guard (user_id fallback only).",
			platformName, chatID, chatType))
	}
	var profile *string
	if v, ok := derived["profile"].(string); ok {
		profile = &v
	}
	return &gateway.SessionSource{Platform: platform, ChatID: chatID, ChatType: chatType, ThreadID: opt("thread_id"),
		UserID: opt("user_id"), UserName: opt("user_name"), ScopeID: scopeID, Profile: profile}, nil
}

// Requeues a refused watch event on a bounded backoff, dropping it after repeated permanent refusals
func requeueRefusedWatchEvent(queue *tools.EventQueue, evt map[string]any) {
	step := pyIntOf(evt["_watch_retry_step"]) + 1
	evt["_watch_retry_step"] = step
	permanent := pyrepr.Truthy(evt["_refusal_permanent"])
	delete(evt, "_refusal_permanent")
	if permanent {
		refusals := pyIntOf(evt["_watch_injection_attempts"]) + 1
		evt["_watch_injection_attempts"] = refusals
		if refusals >= maxCompletionInjectionAttempts {
			sessionID, ok := evt["session_id"]
			if !ok {
				sessionID = "unknown"
			}
			inboundLog().Warn(fmt.Sprintf("Watch notification for %s chat=%s (type=%s session_id=%s) was refused %d times; dropping the queued event (output remains available via process(action='log'))",
				orUnknown(evt["platform"]), orUnknown(evt["chat_id"]), orUnknown(evt["type"]), pyrepr.Str(sessionID), refusals))
			return
		}
	}
	evt["_watch_next_retry_at"] = refusalRetryAt(step)
	queue.Put(evt)
}

// Python int of a falsy or numeric value, zero for a falsy one
func pyIntOf(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// Python str of a value, a question mark for a falsy one
func orUnknown(v any) string {
	if text := truthyText(v); text != "" {
		return text
	}
	return "?"
}

// Connected adapter whose platform value equals the name, native adapters only
func (r *GatewayRunner) adapterByPlatformValue(platformName string) gateway.PlatformAdapter {
	for p, adapter := range r.adapters.All() {
		if string(p) == platformName {
			return adapter
		}
	}
	return nil
}

// Served profile whose own store holds the raw session id, empty for the default profile's self post
func (r *GatewayRunner) servedAPIServerWakeProfile(ctx context.Context, evt map[string]any, rawSID string) (string, error) {
	if r.config == nil || r.config.MultiplexProfiles == nil || !*r.config.MultiplexProfiles {
		return "", nil
	}
	primary := r.primaryProfileName
	if primary == "" {
		primary = "default"
	}
	source, err := r.buildProcessEventSource(ctx, evt)
	if err != nil {
		return "", err
	}
	hinted := ""
	if source != nil && source.Profile != nil {
		hinted = pystr.Strip(*source.Profile)
	}
	if hinted != "" && hinted != primary {
		if gateway.SessionOwnedByProfile(ctx, r.config, hinted, rawSID) {
			return hinted, nil
		}
		return "", &lookupError{fmt.Sprintf("session is not in served profile %s's own store", pyrepr.Repr(hinted))}
	}
	homes, err := gateway.MultiplexProfileHomes(ctx, r.config)
	if err != nil {
		return "", err
	}
	for _, served := range homes {
		if served.Name != primary && gateway.SessionOwnedByProfile(ctx, r.config, served.Name, rawSID) {
			return served.Name, nil
		}
	}
	return "", nil
}

// LookupError of hermes, an event naming a served profile that does not own its session
type lookupError struct{ msg string }

func (e *lookupError) Error() string { return e.msg }

// Adapter for a synthetic event platform, the alias aware resolver first and the literal value scan last
func (r *GatewayRunner) resolveInjectionAdapter(ctx context.Context, platformName string, source *gateway.SessionSource) (gateway.PlatformAdapter, error) {
	if source != nil && gateway.DetachedRouteOf(source) != nil {
		return r.deliveryAdapterFor(ctx, source)
	}
	if source != nil {
		if owner, _, ok := r.transportOwner(source); ok {
			return owner, nil
		}
		if source.DeliveredViaUpstreamRelay {
			relay, _ := r.adapters.Get(gateway.PlatformRelay)
			return relay, nil
		}
	}
	identity := gateway.IdentityOf(source)
	pinned := false
	if identity != nil && identity.Multiplexed && !identity.TransportInferred {
		trusted, err := r.restoredTransportTrusted(ctx, source, identity)
		if err != nil {
			return nil, err
		}
		pinned = trusted
	}
	var profile *string
	if pinned {
		transportProfile := identity.TransportProfile
		profile = &transportProfile
	} else if source != nil {
		profile = source.Profile
	}
	adapters := r.adaptersForProfile(ctx, profile)
	if platform, err := gateway.ParsePlatform(ctx, platformName); err == nil {
		live := make(map[gateway.Platform]any, adapters.Len())
		for p, a := range adapters.All() {
			live[p] = a
		}
		if transport := safeResolveTransport(ctx, platform, r.config, live); transport != nil {
			adapter, _ := transport.Adapter.(gateway.PlatformAdapter)
			return adapter, nil
		}
	}
	if !pinned && source != nil {
		borrowed, err := r.routedPrimaryAdapter(ctx, source)
		if err != nil {
			return nil, err
		}
		if borrowed != nil {
			return borrowed, nil
		}
	}
	if source != nil {
		relay, err := r.committedRelayAdapter(ctx, source)
		if err != nil {
			return nil, err
		}
		if relay != nil {
			return relay, nil
		}
	}
	for p, adapter := range adapters.All() {
		if string(p) == platformName {
			return adapter, nil
		}
	}
	return nil, nil
}

// Delivery transport of a platform, nil when resolving it raises
func safeResolveTransport(ctx context.Context, platform gateway.Platform, cfg *gateway.GatewayConfig, live map[gateway.Platform]any) (transport *gateway.DeliveryTransport) {
	defer func() {
		if recover() != nil {
			transport = nil
		}
	}()
	return gateway.ResolveDeliveryTransport(ctx, platform, cfg, live)
}

// Relay a restored source was admitted through, proven by its route's last committed admission
func (r *GatewayRunner) committedRelayAdapter(ctx context.Context, source *gateway.SessionSource) (gateway.PlatformAdapter, error) {
	authority, err := gateway.ActiveAuthority(ctx, r)
	if err != nil {
		return nil, err
	}
	if authority == nil {
		return nil, nil
	}
	key, err := r.sessionStore.GenerateSessionKey(ctx, source)
	if err != nil {
		return nil, err
	}
	entry := r.sessionStore.LookupBySessionKey(ctx, key)
	if entry == nil {
		return nil, nil
	}
	envelope, err := gateway.CommittedRouteEnvelope(ctx, authority, entry)
	if err != nil || envelope == nil {
		return nil, err
	}
	provenance, _ := envelope.Get("provenance")
	if !pyContains(provenance, "relay") {
		return nil, nil
	}
	native := pyjson.NewObject()
	for _, k := range envelope.Keys {
		if k != "media" {
			v, _ := envelope.Get(k)
			native.Set(k, v)
		}
	}
	payload := pyjson.NewObject()
	payload.Set("text", "")
	payload.Set("native_text_v1", native)
	restored, err := gateway.RestoreNative(ctx, payload, r)
	var refusal *state.RuntimeStoreError
	if errors.As(err, &refusal) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r.AdapterForSource(ctx, restored.Source)
}

// Python membership of an item in a container value
func pyContains(container any, item string) bool {
	switch c := container.(type) {
	case string:
		return strings.Contains(c, item)
	case []any:
		for _, v := range c {
			if v == item {
				return true
			}
		}
	case []string:
		for _, v := range c {
			if v == item {
				return true
			}
		}
	case *pyjson.Object:
		_, ok := c.Get(item)
		return ok
	case map[string]any:
		_, ok := c[item]
		return ok
	}
	return false
}
