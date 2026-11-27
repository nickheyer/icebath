package models

import (
	"context"
	"slices"

	"github.com/nickheyer/icebath/internal/models"
	xaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xai/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type catalog struct{}

func Register(registry *models.Registry) {
	registry.Register(catalog{})
}

func (catalog) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	if route.Spec != xaiconfig.XAIName && route.Spec != xaiconfig.OAuthName {
		return nil, false
	}
	return models.List(ctx, route, "")
}

// Models callable through xAI that models.dev and its own listing leave out
var extras = []string{"grok-4.6", "grok-4.5", "grok-composer-2.5-fast"}

// Headline model kept at the top of the list
const top = "grok-4.6"

// Curated xAI models, every one models.dev knows when it has read them, then what xAI lists; when it lists none, those models.dev knows call
// tools before the curated ones. The models xAI leaves out go after the headline one, which leads
func (c catalog) Catalog(ctx context.Context, env *models.Env) (models.Catalog, bool) {
	route := env.Route
	if route.Spec != xaiconfig.XAIName && route.Spec != xaiconfig.OAuthName {
		return models.Catalog{}, false
	}
	curated := env.Host.Spec(route).StaticModels
	if known := env.Host.DevIDs(env.Config, route, false); len(known) > 0 {
		curated = finalize(known)
	}
	listed, ok := c.Models(ctx, route)
	if !ok || len(listed) == 0 {
		return models.Catalog{IDs: finalize(models.Merge(env.Host.Agentic(env.Config, route, true), curated)), Fallback: true}, true
	}
	return models.Catalog{IDs: models.Merge(curated, listed)}, true
}

// Ids with the models xAI leaves out put after the headline one, which then leads
func finalize(ids []string) []string {
	out := slices.Clone(ids)
	for _, extra := range extras {
		if slices.Contains(out, extra) {
			continue
		}
		at := len(out)
		if len(out) > 0 && out[0] == top {
			at = 1
		}
		out = slices.Insert(out, at, extra)
	}
	if i := slices.Index(out, top); i > 0 {
		out = append([]string{top}, slices.Delete(out, i, i+1)...)
	}
	return out
}
