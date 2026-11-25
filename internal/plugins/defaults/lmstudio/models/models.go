package models

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/models"
	lmstudioconfig "github.com/nickheyer/icebath/internal/plugins/defaults/lmstudio/config"
	"github.com/nickheyer/icebath/internal/provider"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
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
		if !ok || strings.ToLower(strings.TrimSpace(anyutil.StringOr(entry["type"], ""))) == "embedding" {
			continue
		}
		key := entry["key"]
		if !anyutil.IsTrue(key) {
			key = entry["id"]
		}
		if id := strings.TrimSpace(anyutil.StringOr(key, "")); id != "" && !slices.Contains(keys, id) {
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
