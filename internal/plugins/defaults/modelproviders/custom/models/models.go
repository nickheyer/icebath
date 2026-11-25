package models

import (
	"context"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/models"
	customconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/custom/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type catalog struct {
	config *config.Config
}

func Register(registry *models.Registry, cfg *config.Config) {
	registry.Register(catalog{config: cfg})
}

func (c catalog) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	if _, ok := customconfig.Entries(c.config)[route.Provider]; !ok || route.Mode != provider.ModeChatCompletions {
		return nil, false
	}
	return models.List(ctx, route, "")
}
