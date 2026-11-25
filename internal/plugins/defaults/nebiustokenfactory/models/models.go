package models

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/internal/models"
	nebiustokenfactoryconfig "github.com/nickheyer/icebath/internal/plugins/defaults/nebiustokenfactory/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type catalog struct{}

func Register(registry *models.Registry) {
	registry.Register(catalog{})
}

func (catalog) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	if route.Provider != nebiustokenfactoryconfig.Name {
		return nil, false
	}
	return models.List(ctx, route, target(route.BaseURL))
}

func target(base string) string {
	custom := strings.TrimRight(strings.TrimSpace(base), "/")
	if custom == "" || custom == strings.TrimRight(nebiustokenfactoryconfig.Endpoint, "/") {
		return nebiustokenfactoryconfig.ModelsURL
	}
	return custom + "/models"
}
