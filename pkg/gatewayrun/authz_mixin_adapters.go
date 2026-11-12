package gatewayrun

import (
	"context"
	"fmt"
	"weak"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Stripped profile name, empty for an absent or blank name
func stripProfile(profile *string) string {
	if profile == nil {
		return ""
	}
	return pystr.Strip(*profile)
}

// Live adapter whose intake policy gates authorization, nil when the profile has none for the platform
func (r *GatewayRunner) authorizationAdapter(ctx context.Context, platform gateway.Platform, profile *string) gateway.PlatformAdapter {
	if platform == "" {
		return nil
	}
	adapter, _ := r.adaptersForProfile(ctx, profile).Get(platform)
	return adapter
}

// Live adapter map a profile may deliver through, empty for a named profile without one so it never borrows the default bot
func (r *GatewayRunner) adaptersForProfile(ctx context.Context, profile *string) *pydict.Dict[gateway.Platform, gateway.PlatformAdapter] {
	name := stripProfile(profile)
	if name == "" || name == "default" {
		return r.primaryAdapters()
	}
	if adapters, ok := r.profileAdaptersMap().Get(name); ok {
		if cron.HasCredentialPlatform(adapters, nil) || !r.isSharedBotSatellite(ctx, name, "") {
			return adapters
		}
		return pydict.Merge(r.primaryAdapters(), adapters)
	}
	primaryProfile := r.primaryProfileName
	if primaryProfile == "" {
		primaryProfile = r.activeProfileName(ctx)
	}
	if name == primaryProfile {
		return r.primaryAdapters()
	}
	return pydict.New[gateway.Platform, gateway.PlatformAdapter]()
}

// Live adapter map of the profile a source names, the profile adapters seam of the session authority
func (r *GatewayRunner) AdaptersForProfile(profile *string) *pydict.Dict[gateway.Platform, gateway.PlatformAdapter] {
	return r.adaptersForProfile(context.Background(), profile)
}

// Primary transport of a shared bot satellite's routed destination, nil off its routed chats
func (r *GatewayRunner) routedPrimaryAdapter(ctx context.Context, source *gateway.SessionSource) (gateway.PlatformAdapter, error) {
	if source == nil {
		return nil, nil
	}
	platform := source.Platform
	profile := stripProfile(source.Profile)
	if platform == "" || profile == "" || !r.isSharedBotSatellite(ctx, profile, platform) {
		return nil, nil
	}
	if adapters, _ := r.profileAdaptersMap().Get(profile); adapters.Has(platform) {
		return nil, nil
	}
	routed, err := r.defaultBotRoutesTo(ctx, source, profile)
	if err != nil || !routed {
		return nil, err
	}
	transport := gateway.ResolveDeliveryTransport(ctx, platform, r.config, r.primaryAdapters())
	if transport == nil {
		return nil, nil
	}
	adapter, _ := transport.Adapter.(gateway.PlatformAdapter)
	return adapter, nil
}

// Whether the most specific default bot route matching the source's chat names the profile
func (r *GatewayRunner) defaultBotRoutesTo(ctx context.Context, source *gateway.SessionSource, profile string) (bool, error) {
	chatID := source.ChatID
	route, err := gateway.MatchProfileRoute(ctx, r.config.ProfileRoutes, gateway.RouteQuery{Platform: string(source.Platform),
		GuildID: source.ScopeID, ChatID: &chatID, ThreadID: source.ThreadID, ParentChatID: source.ParentChatID, UserID: source.UserID})
	if err != nil {
		return false, err
	}
	return route != nil && route.Profile == profile, nil
}

// Whether a persisted receiving bot can stand in for the live one, a live transport always trusted
func (r *GatewayRunner) restoredTransportTrusted(ctx context.Context, source *gateway.SessionSource, identity *gateway.RoutingIdentity) (bool, error) {
	if identity.Transport != (weak.Pointer[gateway.BasePlatformAdapter]{}) {
		return true, nil
	}
	primary := r.primaryProfileName
	if primary == "" {
		primary = "default"
	}
	if identity.TransportProfile != primary || identity.RuntimeProfile == primary {
		return true, nil
	}
	return r.defaultBotRoutesTo(ctx, source, identity.RuntimeProfile)
}

// Whether a multiplexed identity names the bot that received the source, shared listener mirrors always do
func (r *GatewayRunner) namesReceivingBot(ctx context.Context, source *gateway.SessionSource, identity *gateway.RoutingIdentity) (bool, error) {
	if identity == nil || !identity.Multiplexed || identity.TransportInferred {
		return false, nil
	}
	if gateway.SharedListenerMirrorPlatforms[string(source.Platform)] {
		return true, nil
	}
	return r.restoredTransportTrusted(ctx, source, identity)
}

// Whether a served profile rides the default profile's bot through a profile route, owning no bot on the platform
func (r *GatewayRunner) isSharedBotSatellite(ctx context.Context, profileName string, platform gateway.Platform) bool {
	if !r.multiplexOn() {
		return false
	}
	if owned := r.profileBotPlatforms[profileName]; len(owned) > 0 {
		if _, ownsPlatform := owned[platform]; platform == "" || ownsPlatform {
			return false
		}
	}
	routed := false
	for _, route := range r.config.ProfileRoutes {
		if pyrepr.Truthy(route.Enabled) && route.Profile == profileName && route.BotProfile == nil {
			routed = true
			break
		}
	}
	if !routed {
		return false
	}
	served, err := gateway.MultiplexProfileHomes(ctx, r.config)
	if err != nil {
		return false
	}
	for _, profile := range served {
		if profile.Name == profileName {
			return true
		}
	}
	return false
}

// Adapter that received the source's event, nil without live provenance under multiplexing
func (r *GatewayRunner) intakeAdapterFor(ctx context.Context, source *gateway.SessionSource) (gateway.PlatformAdapter, error) {
	if source == nil {
		return nil, nil
	}
	if adapter, _, ok := r.transportOwner(source); ok {
		return adapter, nil
	}
	if source.DeliveredViaUpstreamRelay {
		relay, _ := r.primaryAdapters().Get(gateway.PlatformRelay)
		return relay, nil
	}
	platform := source.Platform
	identity := gateway.IdentityOf(source)
	names, err := r.namesReceivingBot(ctx, source, identity)
	if err != nil {
		return nil, err
	}
	if names {
		if platform == "" {
			return nil, nil
		}
		transport := identity.TransportProfile
		adapter, _ := r.adaptersForProfile(ctx, &transport).Get(platform)
		return adapter, nil
	}
	if !r.multiplexOn() {
		if platform == "" {
			return nil, nil
		}
		adapter, _ := r.primaryAdapters().Get(platform)
		return adapter, nil
	}
	return nil, nil
}

// Adapter that answers the source, the receiving bot when known and else the unique owner of its platform and profile
func (r *GatewayRunner) deliveryAdapterFor(ctx context.Context, source *gateway.SessionSource) (gateway.PlatformAdapter, error) {
	if source == nil {
		return nil, nil
	}
	if gateway.DetachedRouteOf(source) != nil && *gateway.DetachedRouteOf(source) != "" {
		profile := source.Profile
		if identity := gateway.IdentityOf(source); identity != nil {
			runtime := identity.RuntimeProfile
			profile = &runtime
		}
		adapter, _ := r.adaptersForProfile(ctx, profile).Get(gateway.PlatformLocal)
		return adapter, nil
	}
	adapter, err := r.intakeAdapterFor(ctx, source)
	if err != nil || adapter != nil {
		return adapter, err
	}
	names, err := r.namesReceivingBot(ctx, source, gateway.IdentityOf(source))
	if err != nil || names {
		return nil, err
	}
	if adapter := r.authorizationAdapter(ctx, source.Platform, source.Profile); adapter != nil {
		return adapter, nil
	}
	return r.routedPrimaryAdapter(ctx, source)
}

// Adapter owning the source for the session authority, the delivery seam under its historical name
func (r *GatewayRunner) AdapterForSource(ctx context.Context, source *gateway.SessionSource) (gateway.PlatformAdapter, error) {
	return r.deliveryAdapterFor(ctx, source)
}

// Home whose allowlist admits the source, nil to authorize in the ambient scope
func (r *GatewayRunner) authorizationHomeForSource(ctx context.Context, source *gateway.SessionSource) (*string, error) {
	if identity := gateway.IdentityOf(source); identity != nil {
		if identity.Multiplexed {
			h := identity.AuthorizationHome
			return &h, nil
		}
		return nil, nil
	}
	if stamped := gateway.AuthorizationProfileHomeOf(source); stamped != "" {
		return &stamped, nil
	}
	if !r.multiplexOn() {
		return nil, nil
	}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil || adapter == nil {
		return nil, err
	}
	_, profile := r.owningProfile(adapter, source.Platform)
	if profile == nil {
		h := home.Process()
		return &h, nil
	}
	dir, err := profiles.GetProfileDir(*profile)
	if err != nil {
		return nil, err
	}
	return &dir, nil
}

// Transport owning profile for adapter policy lookups
func (r *GatewayRunner) adapterProfileForSource(source *gateway.SessionSource) *string {
	if _, profile, ok := r.transportOwner(source); ok {
		return profile
	}
	if identity := gateway.IdentityOf(source); identity != nil && identity.Multiplexed {
		if identity.TransportProfile == "default" {
			return nil
		}
		transport := identity.TransportProfile
		return &transport
	}
	return source.Profile
}

// Origin of a routing entry with its identity re-pinned from the persisted receiving bot
func (r *GatewayRunner) restoredSource(ctx context.Context, entry *gateway.SessionEntry) (*gateway.SessionSource, error) {
	if entry == nil || entry.Origin == nil {
		return nil, nil
	}
	if _, err := gateway.RestoreIdentity(ctx, entry.Origin, r, entry.TransportProfile); err != nil {
		return nil, err
	}
	return entry.Origin, nil
}

// Adapter map and gateway config of an adapter's owning profile, the runner's own for the primary profile
func ProfileAdaptersAndConfig(ctx context.Context, runner any, profile string) (*gateway.AdapterMap, *gateway.GatewayConfig, error) {
	r, ok := runner.(*GatewayRunner)
	if !ok {
		return nil, nil, fmt.Errorf("AttributeError: '%T' object has no attribute 'adapters'", runner)
	}
	if profile == "" {
		return r.primaryAdapters(), r.config, nil
	}
	adapters := r.adaptersForProfile(ctx, &profile)
	if adapters == r.primaryAdapters() {
		return adapters, r.config, nil
	}
	dir, err := profiles.GetProfileDir(profile)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := gateway.LoadGatewayConfig(gateway.ProfileRuntimeScope(ctx, dir, true))
	if err != nil {
		return nil, nil, err
	}
	return adapters, cfg, nil
}
