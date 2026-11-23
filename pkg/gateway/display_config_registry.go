package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/settings"
)

// Feeds each registered platform display tier and overrides to the shared display setting resolver
func init() {
	settings.RegisterPlatformTiers(func(ctx context.Context, platformKey string) (string, map[string]any) {
		entry := Registry.Get(ctx, platformKey)
		if entry == nil {
			return "", nil
		}
		return entry.DisplayTier, entry.DisplayOverrides
	})
}
