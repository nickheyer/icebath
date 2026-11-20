package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Registered display label of a platform, its title cased key when none is registered
func platformNoticeLabel(ctx context.Context, platformName string) string {
	if entry := gateway.Registry.Get(ctx, platformName); entry != nil && entry.Label != "" {
		return entry.Label
	}
	return pystr.Title(platformName)
}
