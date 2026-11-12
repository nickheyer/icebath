package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
)

var _ gateway.RunnerIngressContract = (*GatewayRunner)(nil)

// Adapter map of a served profile, created on first use
func (r *GatewayRunner) ProfileAdapterMap(name string) *pydict.Dict[gateway.Platform, gateway.PlatformAdapter] {
	return r.profileAdapterMap(name)
}

// Adapter maps of the served secondary profiles by profile name
func (r *GatewayRunner) ProfileAdapters() *pydict.Dict[string, *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]] {
	return r.profileAdapters
}

// Salted log safe hash of an adapter credential, nil when none is discoverable
func (r *GatewayRunner) AdapterCredentialFingerprint(adapter gateway.PlatformAdapter) *string {
	fingerprint := adapterCredentialFingerprint(adapter)
	if fingerprint == "" {
		return nil
	}
	return &fingerprint
}

// Truth of a named flag on the adapter that authorizes the platform for the profile
func (r *GatewayRunner) AdapterFlag(ctx context.Context, platform gateway.Platform, name string, profile *string) bool {
	return r.adapterFlag(ctx, platform, name, profile)
}

// Profile whose adapter received the source, nil for the launch profile
func (r *GatewayRunner) AdapterProfileForSource(source *gateway.SessionSource) *string {
	return r.adapterProfileForSource(source)
}

// Whether the sender of the source may talk to the agent, adapter delegation honoured when allowed
func (r *GatewayRunner) IsUserAuthorizedForSource(ctx context.Context, source *gateway.SessionSource, allowAdapterDelegation bool) (bool, error) {
	return r.isUserAuthorizedForSource(ctx, source, allowAdapterDelegation)
}

// Runs one inbound message through the gateway turn pipeline and returns its reply
func (r *GatewayRunner) HandleMessage(ctx context.Context, event *gateway.MessageEvent) (any, error) {
	return r.handleMessage(ctx, event)
}

// Clears the durable crash marker the event owns, a failed clear only logged
func (r *GatewayRunner) ClearDurableActiveTurn(ctx context.Context, event *gateway.MessageEvent) error {
	r.clearDurableActiveTurn(ctx, event)
	return nil
}

// Whether a turn holds the running slot of the session key
func (r *GatewayRunner) IsSessionRunning(route string) bool { return r.isSessionRunning(route) }

// Frees the running slot of a route whose agent went idle past its stale bound
func (r *GatewayRunner) EvictIdleStaleAgent(ctx context.Context, route string) {
	r.hmEvictIdleStaleAgent(ctx, route)
}

// Frees the running slot of a route whose stored session was reaped
func (r *GatewayRunner) EvictReapedAgent(ctx context.Context, route string) {
	r.hmEvictReapedAgent(ctx, route)
}

// Profile the home in scope belongs to, default for the root home
func (r *GatewayRunner) ActiveProfileName(ctx context.Context) string {
	return r.activeProfileName(ctx)
}

// Origin of a routing entry with its identity pinned again from the persisted receiving bot
func (r *GatewayRunner) RestoredSource(ctx context.Context, entry *gateway.SessionEntry) (*gateway.SessionSource, error) {
	return r.restoredSource(ctx, entry)
}

// Fallback model chain read again from the config, nil when none is configured
func (r *GatewayRunner) RefreshFallbackModel(ctx context.Context) []map[string]any {
	return r.refreshFallbackModel(ctx)
}

// Gateway prompt of a channel, the agent prompt, the personality and the channel override combined
func (r *GatewayRunner) SystemPromptForChannel(ctx context.Context, platform gateway.Platform, chatID string, threadID, parentID *string) string {
	return r.getSystemPromptForChannel(ctx, platform, chatID, threadID, parentID)
}

// Whether the sender of the source passes the full gateway authorization chain
func (r *GatewayRunner) IsUserAuthorized(ctx context.Context, source *gateway.SessionSource, allowAdapterDelegation bool) (bool, error) {
	return r.isUserAuthorized(ctx, source, allowAdapterDelegation)
}

// Unauthorized DM behavior of the platform for the profile, pair, decline or ignore
func (r *GatewayRunner) GetUnauthorizedDMBehavior(ctx context.Context, platform gateway.Platform, profile *string) string {
	return r.getUnauthorizedDMBehavior(ctx, platform, profile)
}
