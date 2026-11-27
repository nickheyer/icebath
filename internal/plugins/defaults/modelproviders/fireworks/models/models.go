package models

import (
	"context"

	"github.com/nickheyer/icebath/internal/models"
	fireworksconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/fireworks/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type prices struct{}

func Register(registry *models.Registry) {
	registry.Register(models.Listed{Provider: fireworksconfig.Name})
	registry.Register(prices{})
}

// Answered by the listing registered beside it
func (prices) Models(context.Context, provider.Route) ([]string, bool) {
	return nil, false
}

// Prices models.dev keeps for Fireworks, which publishes none of its own to read
func (prices) Prices(_ context.Context, env *models.Env) (map[string]provider.Price, bool) {
	if env.Route.Spec != fireworksconfig.Name {
		return nil, false
	}
	return env.Host.DevPrices(env.Config, env.Route), true
}
