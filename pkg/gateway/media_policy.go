package gateway

import (
	"context"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/settings"
)

// Whether strict media delivery is on for the profile in ctx
func MediaDeliveryStrict(ctx context.Context) bool {
	return settings.GatewayStrict.Get(ctx)
}

// Operator media allowlist of the profile in ctx joined with the path list separator
func MediaDeliveryAllowDirs(ctx context.Context) string {
	return strings.Join(settings.GatewayMediaDeliveryAllowDirs.Strings(ctx), string(os.PathListSeparator))
}

// Whether freshly produced files are trusted for delivery
func MediaDeliveryTrustRecent(ctx context.Context) bool {
	return settings.GatewayTrustRecentFiles.Get(ctx)
}
