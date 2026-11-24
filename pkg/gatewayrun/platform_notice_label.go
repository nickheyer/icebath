package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Registered display label of a platform, its title cased key when none is registered
func platformNoticeLabel(ctx context.Context, platformName string) string {
	if entry := gateway.Registry.Get(ctx, platformName); entry != nil && entry.Label != "" {
		return entry.Label
	}
	return textutil.Title(platformName)
}
