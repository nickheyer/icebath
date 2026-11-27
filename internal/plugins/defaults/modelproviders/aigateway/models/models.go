package models

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/models"
	aigatewayconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/aigateway/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type catalog struct{}

func Register(registry *models.Registry) {
	registry.Register(catalog{})
	registry.Register(models.Listed{Provider: aigatewayconfig.Name})
}

func (catalog) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	key := strings.TrimSpace(route.APIKey)
	if route.Spec != aigatewayconfig.Name || key == "" {
		return nil, false
	}
	headers := map[string]string{"Authorization": "Bearer " + key, "User-Agent": provider.UserAgent("icebath-cli")}
	data, err := models.GetJSON(ctx, strings.TrimRight(strings.TrimSpace(route.BaseURL), "/")+"/models", 5*time.Second, headers)
	if err != nil {
		return nil, false
	}
	document, ok := data.(map[string]any)
	if !ok {
		return nil, false
	}
	items, _ := document["data"].([]any)
	ids := []string{}
	for _, item := range items {
		row, _ := item.(map[string]any)
		id, _ := row["id"].(string)
		tags, _ := row["tags"].([]any)
		if id != "" && row["type"] == "language" && slices.Contains(tags, any("tool-use")) {
			ids = append(ids, id)
		}
	}
	return ids, len(ids) > 0
}

// Prices the AI Gateway lists for its models, its input and output a token
func (catalog) Prices(ctx context.Context, env *models.Env) (map[string]provider.Price, bool) {
	if env.Route.Spec != aigatewayconfig.Name {
		return nil, false
	}
	data, err := models.Request(ctx, "GET", strings.TrimRight(env.Route.BaseURL, "/")+"/models", map[string]string{"Accept": "application/json"}, nil, 8*time.Second)
	if err != nil {
		return nil, true
	}
	prices := map[string]provider.Price{}
	items, _ := data["data"].([]any)
	for _, item := range items {
		row, _ := item.(map[string]any)
		id, _ := row["id"].(string)
		listed, ok := row["pricing"].(map[string]any)
		if id == "" || !ok {
			continue
		}
		r := models.Row{Pricing: map[string]string{"prompt": models.Scalar(listed["input"]), "completion": models.Scalar(listed["output"]),
			"cache_read": models.Scalar(listed["input_cache_read"]), "cache_write": models.Scalar(listed["input_cache_write"])}}
		if price, ok := r.Price(); ok {
			prices[id] = price
		}
	}
	return prices, true
}
