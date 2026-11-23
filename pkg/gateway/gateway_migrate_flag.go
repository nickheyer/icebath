package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Sets gateway.multiplex_profiles in the default profile's config through the settings writer
func writeMultiplexFlag(ctx context.Context, defaultHome string, value bool) error {
	_, err := settings.Set(home.WithOverride(ctx, defaultHome), settings.GatewayMultiplexProfiles.Key, value, settings.Global)
	return err
}
