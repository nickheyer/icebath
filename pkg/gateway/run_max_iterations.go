package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Per turn iteration budget of the profile in ctx, .env reloaded first for rotated keys outside multiplex
func CurrentMaxIterations(ctx context.Context) int {
	if !provider.IsMultiplexActive(ctx) {
		_, _ = provider.LoadIcebathDotenv(ctx, provider.DotenvLoadOptions{Home: home.Dir(context.Background())})
	}
	var raw any
	if n, set := settings.MaxTurns.Lookup(ctx); set {
		raw = n
	}
	return config.ResolveTurnLimit(raw, config.TurnLimitUnlimited)
}
