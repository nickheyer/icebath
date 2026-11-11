package gateway

import "context"

// Frozen launch policy of a local source its adapter admits, nil for other routes
func SessionPolicyForSource(ctx context.Context, runner AuthorityRunner, source *SessionSource) (*LocalSessionPolicy, error) {
	return policyForSource(ctx, runner, source)
}
