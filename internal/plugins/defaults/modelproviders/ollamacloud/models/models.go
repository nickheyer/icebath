package models

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/internal/models"
	ollamacloudconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/ollamacloud/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type catalog struct{}

func Register(registry *models.Registry) {
	registry.Register(catalog{})
}

func (catalog) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	if route.Spec != ollamacloudconfig.Name {
		return nil, false
	}
	return models.List(ctx, route, "")
}

// Models Ollama Cloud lists, then those models.dev knows it serves by their names without the cloud suffix; the latter alone stand in when
// it lists none
func (c catalog) Catalog(ctx context.Context, env *models.Env) (models.Catalog, bool) {
	if env.Route.Spec != ollamacloudconfig.Name {
		return models.Catalog{}, false
	}
	var listed []string
	if env.Route.APIKey != "" {
		listed, _ = c.Models(ctx, env.Route)
	}
	var known []string
	for _, id := range env.Host.Agentic(env.Config, env.Route, true) {
		known = append(known, strings.TrimSuffix(strings.TrimSuffix(id, ":cloud"), "-cloud"))
	}
	return models.Catalog{IDs: models.Merge(listed, known), Fallback: len(listed) == 0}, true
}
