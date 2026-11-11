package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Producers whose profile URL prefix names the profile explicitly on the host's listener
var prefixProducers = map[string]bool{"webhook": true, "api_server": true, "msgraph_webhook": true}

type nativeCallbackKey struct{}

type relayCallbackKey struct{}

// Callback a server created for one native event, its runner, event, resolved transport home and profile
type nativeCallbackBinding struct {
	mu      sync.Mutex
	runner  AuthorityRunner
	event   *MessageEvent
	home    string
	profile *string
}

// Authenticated relay reader binding of one transport to the event it decoded
type relayCallbackBinding struct {
	mu        sync.Mutex
	transport any
	event     any
}

// Websocket relay transport an upstream relay adapter holds
type relayTransport interface {
	RelayGatewayID() string
	RelayUpgradeSecret() string
	RelayAuthRevoked() bool
	RelayClosing() bool
	RelayConnected() bool
	DescriptorForPlatform(platform string) any
	BotIDFor(platform string) any
	RelayURL() string
	RelayIdentities() any
}

// Adapter holding a relay transport
type relayTransportAdapter interface {
	RelayTransport() any
}

// Runner surface native callback provenance binds against
type provenanceRunner interface {
	Adapters() map[Platform]PlatformAdapter
	RunnerConfig() *GatewayConfig
	AdapterCredentialFingerprint(adapter PlatformAdapter) *string
}

// Runner holding the adapter maps of its served secondary profiles
type servedProfileAdaptersRunner interface {
	ProfileAdapters() map[string]map[Platform]PlatformAdapter
}

// Adapter that rechecks a role grant against its platform
type nativeRoleReauthorizer interface {
	ReauthorizeNativeRoles(ctx context.Context, source *SessionSource) (any, error)
}

// Profile key of the transport homes table, the launch profile unnamed
type transportProfileKey struct {
	named bool
	name  string
}

var (
	transportHomesMu sync.Mutex
	transportHomes   = map[any]map[transportProfileKey]*string{}
)

// Table key of an optional profile name
func transportKeyOf(profile *string) transportProfileKey {
	if profile == nil {
		return transportProfileKey{}
	}
	return transportProfileKey{named: true, name: *profile}
}

// Context binding the authenticated relay reader's transport to its event
func RelayCallback(ctx context.Context, transport, event any) context.Context {
	return context.WithValue(ctx, relayCallbackKey{}, &relayCallbackBinding{transport: transport, event: event})
}

// Context moving the reader's relay proof from the raw forward onto its decoded event
func RelayPassthroughEvent(ctx context.Context, transport, forward, event any) context.Context {
	bound, _ := ctx.Value(relayCallbackKey{}).(*relayCallbackBinding)
	if bound == nil {
		return ctx
	}
	bound.mu.Lock()
	matches := bound.transport == transport && bound.event == forward
	bound.mu.Unlock()
	if !matches {
		return ctx
	}
	return RelayCallback(ctx, transport, event)
}

// Records the transport home a server created a callback for, never from event home fields
func RegisterTransportHome(runner AuthorityRunner, profile *string, profileHome *string) error {
	var resolved *string
	if profileHome != nil {
		path, err := pypath.Resolve(*profileHome)
		if err != nil {
			return err
		}
		resolved = &path
	}
	transportHomesMu.Lock()
	defer transportHomesMu.Unlock()
	if transportHomes[runner] == nil {
		transportHomes[runner] = map[transportProfileKey]*string{}
	}
	transportHomes[runner][transportKeyOf(profile)] = resolved
	return nil
}

// Transport home registered for a runner's profile, nil when none is
func transportHomeOf(runner AuthorityRunner, profile *string) *string {
	transportHomesMu.Lock()
	defer transportHomesMu.Unlock()
	return transportHomes[runner][transportKeyOf(profile)]
}

// Context carrying the native callback of one event, refused without a home on an authority runner
func NativeCallback(ctx context.Context, runner AuthorityRunner, event *MessageEvent, transportHome, profile *string) (context.Context, error) {
	if transportHome == nil {
		if bare, ok := runner.(launchAuthorityRunner); ok && bare.SessionAuthority() != nil {
			return nil, runtimeRefusal("profile_mismatch")
		}
		return ctx, nil
	}
	resolved, err := pypath.Resolve(*transportHome)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, nativeCallbackKey{}, &nativeCallbackBinding{runner: runner, event: event, home: resolved, profile: profile}), nil
}

// Moves a callback's provenance onto a rebuilt event that keeps the exact source it authenticated
func CarryCallback(ctx context.Context, event, rewritten *MessageEvent) {
	if native, _ := ctx.Value(nativeCallbackKey{}).(*nativeCallbackBinding); native != nil {
		native.mu.Lock()
		if native.event == event && rewritten.Source == event.Source {
			native.event = rewritten
		}
		native.mu.Unlock()
	}
	if relay, _ := ctx.Value(relayCallbackKey{}).(*relayCallbackBinding); relay != nil {
		relay.mu.Lock()
		if relay.event == any(event) && rewritten.Source == event.Source {
			relay.event = rewritten
		}
		relay.mu.Unlock()
	}
}

// Snapshot of the native callback bound to the context, false when none is
func nativeCallbackOf(ctx context.Context) (AuthorityRunner, *MessageEvent, string, *string, bool) {
	bound, _ := ctx.Value(nativeCallbackKey{}).(*nativeCallbackBinding)
	if bound == nil {
		return nil, nil, "", nil, false
	}
	bound.mu.Lock()
	defer bound.mu.Unlock()
	return bound.runner, bound.event, bound.home, bound.profile, true
}

// Relay transport of an adapter, the AttributeError a Python read of _transport raises when absent
func adapterRelayTransport(adapter PlatformAdapter) (any, error) {
	holder, ok := adapter.(relayTransportAdapter)
	if !ok {
		return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute '_transport'", adapter)}
	}
	return holder.RelayTransport(), nil
}

// Connected relay transport of the adapter and the connector digest pinning its identity
func relayConnector(adapter PlatformAdapter, source *SessionSource) (relayTransport, string, error) {
	var transport relayTransport
	if holder, ok := adapter.(relayTransportAdapter); ok {
		transport, _ = holder.RelayTransport().(relayTransport)
	}
	platform := string(source.Platform)
	if transport == nil || transport.RelayGatewayID() == "" || transport.RelayUpgradeSecret() == "" ||
		transport.RelayAuthRevoked() || transport.RelayClosing() || !transport.RelayConnected() ||
		transport.DescriptorForPlatform(platform) == nil || transport.BotIDFor(platform) == nil {
		return nil, "", runtimeRefusal("permission_denied")
	}
	identity, err := pyjson.Dumps([]any{transport.RelayURL(), transport.RelayGatewayID(), transport.RelayIdentities()},
		pyjson.SortKeys(true), pyjson.Separators(",", ":"))
	if err != nil {
		return nil, "", err
	}
	return transport, relayHMAC(transport.RelayUpgradeSecret(), identity), nil
}

// Hex HMAC SHA256 of text under the relay upgrade secret
func relayHMAC(secret, text string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(text))
	return hex.EncodeToString(mac.Sum(nil))
}

// Seal binding a provenance to the exact source and bot flag it was captured for
func relaySeal(transport relayTransport, source *SessionSource, provenance *pyjson.Object) (string, error) {
	binding, err := pyjson.Dumps([]any{provenance, source.ToDict(), source.IsBot}, pyjson.SortKeys(true), pyjson.Separators(",", ":"))
	if err != nil {
		return "", err
	}
	return relayHMAC(transport.RelayUpgradeSecret(), binding), nil
}

// Provenance rebound to the stored source dict, only for a seal the caller already verified
func resealRelay(ctx context.Context, adapter PlatformAdapter, source, provenance *pyjson.Object) (*pyjson.Object, error) {
	plain, _ := pyjson.Plain(source).(map[string]any)
	stored, err := SessionSourceFromDict(ctx, plain)
	if err != nil {
		return nil, err
	}
	bot, found := source.Get("is_bot")
	if !found {
		return nil, &pyKeyError{"is_bot"}
	}
	stored.IsBot = pyTruthy(bot)
	base := pyjson.NewObject()
	for _, key := range provenance.Keys {
		if key != "relay" {
			value, _ := provenance.Get(key)
			base.Set(key, value)
		}
	}
	raw, err := adapterRelayTransport(adapter)
	if err != nil {
		return nil, err
	}
	transport, ok := raw.(relayTransport)
	if !ok {
		return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute '_upgrade_secret'", raw)}
	}
	seal, err := relaySeal(transport, stored, base)
	if err != nil {
		return nil, err
	}
	resealed := base.Copy()
	resealed.Set("relay", seal)
	return resealed, nil
}

// Multiplexed runtime profile a source routes to, an explicit producer prefix winning over profile routes
func runtimeProfile(ctx context.Context, cfg *GatewayConfig, source *SessionSource, profile *string, ownerName string, homes map[string]string, relay bool) (string, error) {
	chatID := source.ChatID
	query := RouteQuery{Platform: string(source.Platform), GuildID: source.ScopeID, ChatID: &chatID, ThreadID: source.ThreadID,
		ParentChatID: source.ParentChatID, AdapterProfile: profile, UserID: source.UserID}
	var matches []*ProfileRoute
	for _, route := range cfg.ProfileRoutes {
		matched, err := route.Matches(ctx, query)
		if err != nil {
			return "", err
		}
		if matched {
			matches = append(matches, route)
		}
	}
	if source.Profile != nil && *source.Profile != "" && prefixProducers[string(source.Platform)] {
		return *source.Profile, nil
	}
	if len(matches) > 0 {
		rank := matches[0].Specificity()
		for _, route := range matches[1:] {
			rank = max(rank, route.Specificity())
		}
		var best []*ProfileRoute
		for _, route := range matches {
			if route.Specificity() == rank {
				best = append(best, route)
			}
		}
		if len(best) != 1 {
			return "", runtimeRefusal("admission_conflict")
		}
		return best[0].Profile, nil
	}
	if relay && profile == nil && source.Profile != nil {
		if _, served := homes[*source.Profile]; served {
			return *source.Profile, nil
		}
	}
	return ownerName, nil
}

// True when the path names an existing directory
func isDirPath(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// Registered adapter, transport home, runtime home and provenance a native source binds to, refused when unproven
func provenanceBinding(ctx context.Context, runner AuthorityRunner, source *SessionSource, profile *string) (PlatformAdapter, string, string, *pyjson.Object, error) {
	refuse := func(reason string) (PlatformAdapter, string, string, *pyjson.Object, error) {
		return nil, "", "", nil, runtimeRefusal(reason)
	}
	bound, ok := runner.(provenanceRunner)
	if !ok {
		return nil, "", "", nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute 'adapters'", runner)}
	}
	type registry struct {
		owner   *string
		mapping map[Platform]PlatformAdapter
	}
	registries := []registry{{nil, bound.Adapters()}}
	if secondary, ok := runner.(servedProfileAdaptersRunner); ok {
		for name, mapping := range secondary.ProfileAdapters() {
			owner := name
			registries = append(registries, registry{&owner, mapping})
		}
	}
	relay := source.DeliveredViaUpstreamRelay
	platform := source.Platform
	if relay {
		platform = PlatformRelay
	}
	var adapter PlatformAdapter
	for _, item := range registries {
		if (item.owner == nil) == (profile == nil) && (item.owner == nil || *item.owner == *profile) {
			adapter = item.mapping[platform]
			break
		}
	}
	if adapter == nil {
		return refuse("not_found")
	}
	same := 0
	for _, item := range registries {
		if item.mapping[platform] == adapter {
			same++
		}
	}
	if same != 1 {
		return refuse("not_found")
	}
	transportHome := transportHomeOf(runner, profile)
	if transportHome == nil || !isDirPath(*transportHome) {
		return refuse("profile_mismatch")
	}
	homePath := *transportHome
	runtimeHome := homePath
	cfg := bound.RunnerConfig()
	multiplex := cfg != nil && cfg.MultiplexProfiles != nil && *cfg.MultiplexProfiles
	if !multiplex && source.Profile != nil && *source.Profile != "" {
		return refuse("profile_mismatch")
	}
	if multiplex {
		if cfg.runtimeProfileHomes == nil {
			return refuse("profile_mismatch")
		}
		homes := map[string]string{}
		seen := map[string]bool{}
		for _, served := range *cfg.runtimeProfileHomes {
			canonical, err := pypath.Resolve(served.Home)
			if err != nil {
				return nil, "", "", nil, err
			}
			if _, dup := homes[served.Name]; dup || seen[canonical] {
				return refuse("profile_mismatch")
			}
			homes[served.Name] = canonical
			seen[canonical] = true
		}
		launch, err := pypath.Resolve(home.Process())
		if err != nil {
			return nil, "", "", nil, err
		}
		primary := ""
		for _, served := range *cfg.runtimeProfileHomes {
			if homes[served.Name] == launch {
				primary = served.Name
				break
			}
		}
		if primary == "" {
			return refuse("profile_mismatch")
		}
		ownerName := primary
		if profile != nil && *profile != "" {
			ownerName = *profile
		}
		if owned, ok := homes[ownerName]; !ok || owned != homePath {
			return refuse("profile_mismatch")
		}
		routed, err := runtimeProfile(ctx, cfg, source, profile, ownerName, homes, relay)
		if err != nil {
			return nil, "", "", nil, err
		}
		named := primary
		if source.Profile != nil && *source.Profile != "" {
			named = *source.Profile
		}
		routedHome, served := homes[routed]
		if named != routed || !served {
			return refuse("profile_mismatch")
		}
		runtimeHome = routedHome
		if !isDirPath(runtimeHome) {
			return refuse("profile_mismatch")
		}
	}
	authority, err := authorityForHome(runner, runtimeHome)
	if err != nil {
		return nil, "", "", nil, err
	}
	if authority == nil {
		return refuse("profile_mismatch")
	}
	var transport relayTransport
	var connector string
	if relay {
		if transport, connector, err = relayConnector(adapter, source); err != nil {
			return nil, "", "", nil, err
		}
	} else if fingerprint := bound.AdapterCredentialFingerprint(adapter); fingerprint != nil && *fingerprint != "" {
		connector = *fingerprint
	} else {
		owner := ""
		if profile != nil {
			owner = *profile
		}
		connector = "adapter:" + string(source.Platform) + ":" + owner
	}
	provenance := pyjson.ObjectOf("transport_home", homePath, "runtime_home", runtimeHome, "platform", string(source.Platform), "connector", connector)
	if multiplex {
		if profile != nil {
			provenance.Set("transport_profile", *profile)
		} else {
			provenance.Set("transport_profile", nil)
		}
	}
	if relay {
		seal, err := relaySeal(transport, source, provenance)
		if err != nil {
			return nil, "", "", nil, err
		}
		provenance.Set("relay", seal)
	}
	return adapter, homePath, runtimeHome, provenance, nil
}

// Provenance of the native callback event a server created, nil for an event no callback carries
func captureProvenance(ctx context.Context, runner AuthorityRunner, event *MessageEvent) (*pyjson.Object, error) {
	boundRunner, boundEvent, boundHome, profile, ok := nativeCallbackOf(ctx)
	if !ok || boundRunner != runner || boundEvent != event {
		return nil, nil
	}
	adapter, transportHome, _, provenance, err := provenanceBinding(ctx, runner, event.Source, profile)
	if err != nil {
		return nil, err
	}
	var owner PlatformAdapter
	owned := false
	if event.Source.DeliveredViaUpstreamRelay {
		delivery, _ := ctx.Value(relayCallbackKey{}).(*relayCallbackBinding)
		if delivery == nil {
			return nil, runtimeRefusal("permission_denied")
		}
		transport, err := adapterRelayTransport(adapter)
		if err != nil {
			return nil, err
		}
		delivery.mu.Lock()
		matches := delivery.transport == transport && delivery.event == any(event)
		delivery.mu.Unlock()
		if !matches {
			return nil, runtimeRefusal("permission_denied")
		}
		owner, owned = adapter, true
	} else {
		resolver, can := runner.(TransportOwnerRunner)
		if !can {
			return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute '_transport_owner'", runner)}
		}
		owner, _, owned = resolver.TransportOwner(event.Source)
	}
	if !owned || owner != adapter || transportHome != boundHome {
		return nil, runtimeRefusal("not_found")
	}
	return provenance, nil
}

// Rebinds a source to the connector and home its provenance names today, returning the runtime home
func restoreProvenance(ctx context.Context, runner AuthorityRunner, source *SessionSource, provenance any) (*string, error) {
	recorded, ok := provenance.(*pyjson.Object)
	if !ok {
		if plain, isMap := provenance.(map[string]any); isMap {
			recorded = pyjson.NewObject()
			for key, value := range plain {
				recorded.Set(key, value)
			}
		} else {
			return nil, runtimeRefusal("invalid_params")
		}
	}
	candidate := *source
	_, sealed := recorded.Get("relay")
	candidate.DeliveredViaUpstreamRelay = sealed
	var profile *string
	transportProfile := ""
	if raw, _ := recorded.Get("transport_profile"); raw != nil {
		name, isText := raw.(string)
		if !isText {
			return nil, runtimeRefusal("not_found")
		}
		profile, transportProfile = &name, name
	}
	adapter, transportHome, runtimeHome, expected, err := provenanceBinding(ctx, runner, &candidate, profile)
	if err != nil {
		return nil, err
	}
	if !pyrepr.Equal(recorded, expected) {
		return nil, runtimeRefusal("profile_mismatch")
	}
	source.DeliveredViaUpstreamRelay = candidate.DeliveredViaUpstreamRelay
	source.SetTransportAdapterRef(adapter)
	owners, _ := runner.(TransportOwnerRunner)
	if _, err := canonicalIdentity(ctx, source, owners, adapter, transportProfile); err != nil {
		return nil, err
	}
	source.authorizationProfileHome = transportHome
	return &runtimeHome, nil
}

// True when a role flag's connector check grants the sender, the flag itself never granting permission
func reauthorizeRoles(ctx context.Context, runner AuthorityRunner, source *SessionSource, provenance any) (bool, error) {
	if !source.RoleAuthorized {
		return false, nil
	}
	if provenance == nil || source.DeliveredViaUpstreamRelay {
		return false, runtimeRefusal("invalid_params")
	}
	if _, err := restoreProvenance(ctx, runner, source, provenance); err != nil {
		return false, err
	}
	adapter := runner.AdapterForSource(ctx, source)
	authz, ok := runner.(nativeAuthorizationRunner)
	if !ok {
		return false, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute '_is_user_authorized_for_source'", runner)}
	}
	direct, err := authz.IsUserAuthorizedForSource(ctx, source, false)
	if err != nil || direct {
		return direct, err
	}
	check, ok := adapter.(nativeRoleReauthorizer)
	if !ok {
		return false, runtimeRefusal("permission_denied")
	}
	recorded, _ := provenance.(*pyjson.Object)
	transportHome := ""
	if recorded != nil {
		value, found := recorded.Get("transport_home")
		if !found {
			return false, &pyKeyError{"transport_home"}
		}
		transportHome = pyrepr.Str(value)
	} else if plain, isMap := provenance.(map[string]any); isMap {
		transportHome = pyrepr.Str(plain["transport_home"])
	}
	allowed, err := check.ReauthorizeNativeRoles(ProfileRuntimeScope(ctx, transportHome, true), source)
	if err != nil {
		return false, err
	}
	if _, err := restoreProvenance(ctx, runner, source, provenance); err != nil {
		return false, err
	}
	if runner.AdapterForSource(ctx, source) != adapter {
		return false, runtimeRefusal("not_found")
	}
	if allowed != true {
		return false, runtimeRefusal("permission_denied")
	}
	return true, nil
}

// Runner of the native callback bound to the context, nil outside one
func callbackRunner(ctx context.Context) AuthorityRunner {
	runner, _, _, _, _ := nativeCallbackOf(ctx)
	return runner
}
