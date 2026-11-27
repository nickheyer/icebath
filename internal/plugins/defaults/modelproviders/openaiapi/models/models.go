package models

import (
	"context"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/models"
	openaiapiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaiapi/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type catalog struct{}

func Register(registry *models.Registry) {
	registry.Register(catalog{})
}

func (catalog) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	if route.Spec != openaiapiconfig.Name {
		return nil, false
	}
	return models.List(ctx, route, "")
}

// Agentic models of OpenAI's own API: the curated ones it lists for the key, with Astra when the key reaches it; the curated ones when it
// lists none of them, and all it lists on another host
func (c catalog) Catalog(ctx context.Context, env *models.Env) (models.Catalog, bool) {
	route := env.Route
	if route.Spec != openaiapiconfig.Name || route.APIKey == "" {
		return models.Catalog{}, false
	}
	listed, ok := c.Models(ctx, route)
	if !ok || len(listed) == 0 {
		return models.Catalog{}, false
	}
	if host := provider.BaseHost(route.BaseURL); host != "api.openai.com" {
		return models.Catalog{IDs: listed}, true
	}
	curated := env.Host.Spec(route).StaticModels
	var found []string
	for _, id := range curated {
		if slices.ContainsFunc(listed, func(have string) bool { return strings.EqualFold(have, id) }) {
			found = append(found, id)
		}
	}
	for _, id := range listed {
		if lower := strings.ToLower(id); lower == "gpt-6-astra" || lower == "gpt-6-astra-900k" {
			found = append(found, id)
		}
	}
	switch {
	case len(found) > 0:
		return models.Catalog{IDs: found}, true
	case len(curated) > 0:
		return models.Catalog{IDs: curated}, true
	}
	return models.Catalog{IDs: listed}, true
}
