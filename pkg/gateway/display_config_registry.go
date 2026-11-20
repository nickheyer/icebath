package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
)

// Feeds each registered platform display tier and overrides to the shared display setting resolver
func init() {
	config.PlatformDisplayLookup = func(ctx context.Context, platformKey string) (string, map[string]any) {
		entry := Registry.Get(ctx, platformKey)
		if entry == nil {
			return "", nil
		}
		return entry.DisplayTier, entry.DisplayOverrides
	}
}
