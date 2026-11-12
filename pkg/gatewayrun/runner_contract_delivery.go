package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/plugins"
)

var (
	_ gateway.RunnerDeliveryContract = (*GatewayRunner)(nil)
	_ plugins.PlatformActionRunner   = (*GatewayRunner)(nil)
	_ cron.GatewayRunner             = (*GatewayRunner)(nil)
)

// Replays the failed obligations of one bot identity after its adapter reconnected, the count redelivered
func (r *GatewayRunner) RedeliverFailedObligationsForPlatform(ctx context.Context, platform gateway.Platform, profile *string) int {
	return r.redeliverFailedObligationsForPlatform(ctx, platform, profile)
}

// Wakes or starts the deadline driven ledger worker of one bot identity after a refused final
func (r *GatewayRunner) ScheduleFloodRedelivery(ctx context.Context, platform gateway.Platform, profile *string) {
	r.scheduleFloodRedelivery(ctx, platform, profile)
}

// Profile a configured route sends the source to, nil without a match, a refused route raising ProfileRouteRejected
func (r *GatewayRunner) ProfileNameForSource(ctx context.Context, source *gateway.SessionSource, adapterProfile *string) (*string, error) {
	return r.profileNameForSource(ctx, source, adapterProfile)
}

// Adapter that answers the source, the receiving bot when known and else its unique owner
func (r *GatewayRunner) DeliveryAdapterFor(ctx context.Context, source *gateway.SessionSource) (gateway.PlatformAdapter, error) {
	return r.deliveryAdapterFor(ctx, source)
}

// Metadata platforms need for a thread aware reply to the source, nil without thread or route state
func (r *GatewayRunner) ThreadMetadataForSource(source *gateway.SessionSource, replyToMessageID *string) map[string]any {
	return r.threadMetadataForSource(source, replyToMessageID)
}

// Current session key of the source, the store derivation first and the configured fallback after
func (r *GatewayRunner) SessionKeyForSource(ctx context.Context, source *gateway.SessionSource) (string, error) {
	return r.sessionKeyForSource(ctx, source)
}

// Live local session the plugin profile's own authority holds under the key, nil when it holds none
func (r *GatewayRunner) PluginLocalSession(ctx context.Context, source *gateway.SessionSource, sessionKey, pluginHome, pluginID string) (*gateway.LiveSession, error) {
	return r.pluginLocalSession(ctx, source, sessionKey, pluginHome, pluginID)
}

// Whether a messaging session belongs to the plugin's own profile, true for a caller without a home
func (r *GatewayRunner) PluginMessagingSessionOwned(ctx context.Context, sessionKey, pluginHome, pluginID string) bool {
	return r.pluginMessagingSessionOwned(ctx, sessionKey, pluginHome, pluginID)
}

// Event loop the runner serves on, nil until gateway startup binds one and after the runtime resets it
func (r *GatewayRunner) GatewayLoop() gateway.EventLoop { return r.gatewayLoop }

// Whether ctx runs as a task of the runner's gateway loop rather than on a worker thread
func (r *GatewayRunner) OnGatewayLoop(ctx context.Context) bool {
	return r.gatewayLoop != nil && onGatewayLoop(ctx)
}

// Enum value gateway.config.Platform accepts for the platform name, a ValueError for any other value given
func (r *GatewayRunner) ParsePlatform(ctx context.Context, value string) (string, error) {
	platform, err := gateway.ParsePlatform(ctx, value)
	if err != nil {
		return "", err
	}
	return string(platform), nil
}

// Whether a served profile rides the default bot through a profile route, owning no bot on the platform
func (r *GatewayRunner) IsSharedBotSatellite(ctx context.Context, profileName string, platform gateway.Platform) bool {
	return r.isSharedBotSatellite(ctx, profileName, platform)
}
