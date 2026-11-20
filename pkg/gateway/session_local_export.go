package gateway

import "context"

// Stages, commits and publishes a local session inline for synchronous callers with no trusted policy
func CreateLocalSession(ctx context.Context, a *SessionAuthority, actor Principal, params map[string]any) (SessionRef, error) {
	return createLocalSession(ctx, a, actor, params, localCreateOptions{})
}

// Frozen launch policy of a local source its adapter admits, nil for other routes
func SessionPolicyForSource(ctx context.Context, runner AuthorityRunner, source *SessionSource) (*LocalSessionPolicy, error) {
	return policyForSource(ctx, runner, source)
}
