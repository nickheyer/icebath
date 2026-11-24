package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/settings"
)

// Whether strict media delivery is on for the profile in ctx
func MediaDeliveryStrict(ctx context.Context) bool {
	return settings.GatewayStrict.Get(ctx)
}

// Whether freshly produced files are trusted for delivery
func MediaDeliveryTrustRecent(ctx context.Context) bool {
	return settings.GatewayTrustRecentFiles.Get(ctx)
}
