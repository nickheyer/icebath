package cron

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Store over the profile home for external test packages
func OpenStoreForTest(home string) *Store { return openCronStore(home, nil) }

// Platform config the transport resolver picked for a chat target and its refusal reason
func (s *Store) ResolvedTransportConfigForTest(ctx context.Context, platform gateway.Platform, name, chatID string, adapters any, cfg *gateway.GatewayConfig) (*gateway.PlatformConfig, string) {
	resolved, reason := s.resolveTargetTransport(ctx, platform, name, Target{ChatID: chatID}, adapters, cfg)
	if resolved == nil {
		return nil, reason
	}
	return resolved.pconfig, reason
}
