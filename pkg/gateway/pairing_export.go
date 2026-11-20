package gateway

import "context"

// Allowlist env var that grants a platform user access, empty when the platform has none
func AllowlistEnvForPlatform(ctx context.Context, platform string) string {
	return allowlistEnvForPlatform(ctx, platform)
}

// Whether the user is inside the pairing request cooldown of the platform
func (s *PairingStore) IsRateLimited(ctx context.Context, platform, userID string) (bool, error) {
	return s.isRateLimited(ctx, platform, userID)
}

// Starts the pairing request cooldown of the user on the platform
func (s *PairingStore) RecordRateLimit(ctx context.Context, platform, userID string) error {
	return s.recordRateLimit(ctx, platform, userID)
}
