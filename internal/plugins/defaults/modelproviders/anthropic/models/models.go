package models

import (
	"cmp"
	"context"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/models"
	anthropicconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/strs"
)

const maxPages = 20

type catalog struct{}

func Register(registry *models.Registry) {
	registry.Register(catalog{})
}

func (catalog) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	if route.Spec != anthropicconfig.Name || route.APIKey == "" {
		return nil, false
	}
	headers := map[string]string{"anthropic-version": "2023-06-01", "Accept": "application/json"}
	if route.Auth == provider.AuthOAuth {
		headers["Authorization"] = "Bearer " + route.APIKey
		headers["anthropic-beta"] = "interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14,claude-code-20250219,oauth-2025-04-20"
	} else {
		headers["x-api-key"] = route.APIKey
	}
	ids, seen, cursor := []string{}, map[string]bool{}, ""
	for range maxPages {
		data, err := models.GetJSON(ctx, target(route.BaseURL, cursor), 8*time.Second, headers)
		if err != nil {
			return nil, false
		}
		document, ok := data.(map[string]any)
		if !ok {
			return nil, false
		}
		items, present := document["data"]
		list, ok := items.([]any)
		if present && !ok {
			return nil, false
		}
		for _, item := range list {
			if row, ok := item.(map[string]any); ok {
				if id, ok := row["id"].(string); ok {
					ids = append(ids, id)
				}
			}
		}
		if cursor = next(document, seen); cursor == "" {
			break
		}
	}
	return strs.Unique(ids), true
}

func target(base, after string) string {
	endpoint := strings.TrimRight(strings.TrimSpace(base), "/")
	address := endpoint + "/v1/models"
	if strings.HasSuffix(endpoint, "/v1") {
		address = endpoint + "/models"
	}
	query := url.Values{"limit": {"1000"}}
	if after != "" {
		query.Set("after_id", after)
	}
	return address + "?" + query.Encode()
}

func next(page map[string]any, seen map[string]bool) string {
	last, _ := page["last_id"].(string)
	if page["has_more"] != true || last == "" || seen[last] {
		return ""
	}
	seen[last] = true
	return last
}

// Window the models listing gives the route's model, for a Console API key; an OAuth token can't read it
func (catalog) ContextLength(ctx context.Context, env *models.Env) (int, bool) {
	route := env.Route
	if route.Spec != anthropicconfig.Name && provider.BaseHost(route.BaseURL) != "api.anthropic.com" || route.APIKey == "" ||
		strings.HasPrefix(route.APIKey, "sk-ant-oat") || route.Auth == provider.AuthOAuth {
		return 0, false
	}
	base := strings.TrimSuffix(strings.TrimRight(cmp.Or(route.BaseURL, anthropicconfig.BaseURL), "/"), "/v1")
	data, err := models.Request(ctx, "GET", base+"/v1/models?limit=1000", map[string]string{"x-api-key": route.APIKey, "anthropic-version": "2023-06-01"},
		nil, 10*time.Second)
	if err != nil {
		slog.Debug("Anthropic /v1/models query failed", "error", err)
		return 0, false
	}
	items, _ := data["data"].([]any)
	for _, item := range items {
		row, _ := item.(map[string]any)
		if row["id"] == route.Model {
			if n, ok := row["max_input_tokens"].(float64); ok && n > 0 {
				return int(n), true
			}
		}
	}
	return 0, false
}

// Curated models first, then those the API lists; only what it lists on an address of one's own, the curated ones standing in when it
// lists none
func (c catalog) Catalog(ctx context.Context, env *models.Env) (models.Catalog, bool) {
	route := env.Route
	if route.Spec != anthropicconfig.Name {
		return models.Catalog{}, false
	}
	curated := env.Host.Spec(route).StaticModels
	listed, ok := c.Models(ctx, route)
	switch {
	case !ok || len(listed) == 0:
		return models.Catalog{IDs: curated, Fallback: true}, true
	case strings.TrimRight(route.BaseURL, "/") != strings.TrimRight(anthropicconfig.BaseURL, "/"):
		return models.Catalog{IDs: listed}, true
	}
	return models.Catalog{IDs: models.Merge(curated, listed)}, true
}
