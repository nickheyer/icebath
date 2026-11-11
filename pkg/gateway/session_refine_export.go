package gateway

import "context"

// Physical transcript a local session's history reads, its compression continuation when rotated
func TranscriptTarget(ctx context.Context, a *SessionAuthority, ref SessionRef) (string, error) {
	return transcriptTarget(ctx, a, ref)
}

// Retires a closed local session once its review finished, a no op while the session is still open
func RetireAfterReview(ctx context.Context, a *SessionAuthority, ref SessionRef, live *LiveSession) error {
	return retireAfterReview(ctx, a, ref, live)
}

// Next turn's toolsets from its resolved config, the profile's current pick when following
func (p *LocalSessionPolicy) CurrentToolsets(ctx context.Context, cfg map[string]any) ([]string, error) {
	return p.currentToolsets(ctx, cfg)
}
