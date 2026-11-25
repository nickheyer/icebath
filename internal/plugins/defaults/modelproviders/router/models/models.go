package models

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/models"
	routerconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/router/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type catalog struct{}

func Register(registry *models.Registry) {
	registry.Register(catalog{})
}

func (catalog) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	if route.Provider != routerconfig.Name {
		return nil, false
	}
	headers := map[string]string{"Accept": "application/json", "User-Agent": models.UserAgent}
	if route.APIKey != "" {
		headers["Authorization"] = "Bearer " + route.APIKey
	}
	data, err := models.GetJSON(ctx, strings.TrimRight(route.BaseURL, "/")+"/models", 8*time.Second, headers)
	if err != nil {
		return nil, false
	}
	items := data
	if document, ok := data.(map[string]any); ok {
		if items, ok = document["data"]; !ok {
			items = []any{}
		}
	}
	list, ok := items.([]any)
	if !ok {
		return nil, false
	}
	ids := []string{}
	for _, item := range list {
		row, _ := item.(map[string]any)
		if id, _ := row["id"].(string); id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids, len(ids) > 0
}
