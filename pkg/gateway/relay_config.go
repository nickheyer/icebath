package gateway

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/settings"
)

// Text of a relay setting for ctx, stripped and empty when unset
func relaySetting(ctx context.Context, s *settings.String) string {
	return strings.TrimSpace(s.Get(ctx))
}

// URL valued relay setting with trailing slashes stripped, nil when unset
func relaySettingURL(ctx context.Context, s *settings.String) *string {
	value := strings.TrimRight(relaySetting(ctx, s), "/")
	if value == "" {
		return nil
	}
	return &value
}

// Wake URL the connector pokes to revive a suspended gateway, nil when none is registered
func RelayWakeURL(ctx context.Context) *string {
	return relaySettingURL(ctx, settings.GatewayRelayWakeURL)
}
