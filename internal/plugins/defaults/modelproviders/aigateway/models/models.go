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
	headers := map[string]string{"Authorization": "Bearer " + key, "User-Agent": models.UserAgent}
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
