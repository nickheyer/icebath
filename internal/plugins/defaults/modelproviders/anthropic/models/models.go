package models

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/models"
	anthropicconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/config"
	"github.com/nickheyer/icebath/internal/provider"
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
	return unique(ids), true
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

func unique(ids []string) []string {
	seen, out := map[string]bool{}, []string{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
