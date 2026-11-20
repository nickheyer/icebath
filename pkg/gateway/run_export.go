package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/profiles"
)

// Session state a runner peeks without creating, as session info reads it
type PeekedSessionState = peekedSessionState

// Session key the store derives for source
func (s *SessionStore) GenerateSessionKey(ctx context.Context, source *SessionSource) (string, error) {
	return s.generateSessionKey(ctx, source, nil)
}

// Profile model of a config or the gateway config, else the claimed welcome trial model
func ResolveGatewayModel(ctx context.Context, cfg map[string]any) string {
	return resolveGatewayModel(ctx, cfg)
}

// Frozen policy of a local session, nil for a messaging chat
func LocalPolicy(ctx context.Context, runner AuthorityRunner, source *SessionSource) (*LocalSessionPolicy, error) {
	return localPolicy(ctx, runner, source)
}

// Local live session whose route is the session key, nil when none is
func LocalLiveForRoute(runner any, sessionKey string) *LiveSession {
	return localLiveForRoute(runner, sessionKey)
}

// Frozen policy of a local source its adapter admits, nil for other routes
func LocalPolicyForSource(ctx context.Context, runner AuthorityRunner, source *SessionSource) (*LocalSessionPolicy, error) {
	return policyForSource(ctx, runner, source)
}

// Platform block of a config the loader picks, a top level block first
func PlatformSection(yamlCfg map[string]any, name string, gatewayPlatforms any) (any, bool) {
	return platformSection(yamlCfg, name, gatewayPlatforms)
}

// Authority of the active runtime scope of a runner
func ActiveAuthority(ctx context.Context, runner any) (*SessionAuthority, error) {
	return activeAuthority(ctx, runner)
}

// Profiles this multiplexer serves, the boot reservation once taken, else the live profiles directory
func MultiplexProfileHomes(ctx context.Context, cfg *GatewayConfig) ([]profiles.ServedProfile, error) {
	return multiplexProfileHomes(ctx, cfg)
}
