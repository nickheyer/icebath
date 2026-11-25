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
}

func (catalog) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	if route.Provider != lmstudioconfig.Name {
		return nil, false
	}
	root := serverRoot(route.BaseURL)
	if root == "" {
		return nil, false
	}
	headers := map[string]string{"User-Agent": "icebath-cli/dev"}
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

func serverRoot(base string) string {
	root := strings.TrimRight(strings.TrimSpace(base), "/")
	for _, suffix := range []string{"/api/v1", "/api", "/v1"} {
		if strings.HasSuffix(root, suffix) {
			return strings.TrimRight(strings.TrimSuffix(root, suffix), "/")
		}
	}
	return root
}
