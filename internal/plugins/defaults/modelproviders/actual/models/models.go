package models

import (
	"context"

	"github.com/nickheyer/icebath/internal/models"
	actualconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/actual/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type catalog struct{}

func Register(registry *models.Registry) {
	registry.Register(catalog{})
}

func (catalog) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	if route.Spec != actualconfig.Name {
		return nil, false
	}
	return models.List(ctx, route, actualconfig.NormalizeBaseURL(route.BaseURL)+"/models")
}
