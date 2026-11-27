package models

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/models"
	lmstudioconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/lmstudio/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type catalog struct{}

func Register(registry *models.Registry) {
	registry.Register(catalog{})
	registry.Register(&reasoning{held: map[string]options{}})
}

func (catalog) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	if route.Spec != lmstudioconfig.Name {
		return nil, false
	}
	root := models.LMStudioRoot(route.BaseURL)
	if root == "" {
		return nil, false
	}
	headers := map[string]string{"User-Agent": provider.UserAgent("icebath-cli")}
	if token := strings.TrimSpace(route.APIKey); token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	data, err := models.GetJSON(ctx, root+"/api/v1/models", 5*time.Second, headers)
	if err != nil {
		return nil, false
	}
	document, _ := data.(map[string]any)
	entries, ok := document["models"].([]any)
	if !ok {
		return nil, false
	}
	keys := []string{}
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if kind, _ := entry["type"].(string); !ok || strings.ToLower(strings.TrimSpace(kind)) == "embedding" {
			continue
		}
		key, _ := entry["key"].(string)
		if key == "" {
			key, _ = entry["id"].(string)
		}
		if id := strings.TrimSpace(key); id != "" && !slices.Contains(keys, id) {
			keys = append(keys, id)
		}
	}
	return keys, true
}
