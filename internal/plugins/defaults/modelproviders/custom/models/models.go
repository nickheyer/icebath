package models

import (
	"context"

	"github.com/nickheyer/icebath/internal/models"
	customconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/custom/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type catalog struct{}

func Register(registry *models.Registry) {
	registry.Register(catalog{})
}

func (catalog) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	if route.Spec != customconfig.Name || route.Mode != provider.ModeChatCompletions {
		return nil, false
	}
	return models.List(ctx, route, "")
}
