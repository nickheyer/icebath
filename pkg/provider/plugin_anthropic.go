package provider

import (
	"context"
	"log/slog"
	"time"
)

// Profiles the bundled anthropic plugin registers
func anthropicPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "anthropic", mode: ModeAnthropicMessages,
		keys: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}, base: "https://api.anthropic.com",
		authType: AuthTypeAPIKey}, "claude", "claude-oauth", "claude-code")
	p.DefaultAuxModel = "claude-haiku-4-5-20251001"
	p.FetchModelsHook = anthropicProfileFetchModels
	return []Profile{p}
}

// Model ids over every cursor page of the models endpoint with the x-api-key header, false without a key or on failure
func anthropicProfileFetchModels(ctx context.Context, apiKey, baseURL string, timeout time.Duration) ([]string, bool) {
	if apiKey == "" {
		return nil, false
	}
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	headers := map[string]string{"x-api-key": apiKey, "anthropic-version": "2023-06-01", "Accept": "application/json"}
	var models []string
	seenCursors := map[string]bool{}
	cursor := ""
	for range anthropicModelsMaxPages {
		data, err := modelsGetJSON(ctx, anthropicModelsURL(baseURL, cursor), timeout, headers, nil)
		if err != nil {
			slog.Debug("fetch_models(anthropic): " + err.Error())
			return nil, false
		}
		doc, isDict := data.(map[string]any)
		if !isDict {
			slog.Debug("fetch_models(anthropic): '" + pyTypeName(data) + "' object has no attribute 'get'")
			return nil, false
		}
		items, present := doc["data"]
		if !present {
			items = []any{}
		}
		list, isList := items.([]any)
		if !isList {
			slog.Debug("fetch_models(anthropic): '" + pyTypeName(items) + "' object is not iterable")
			return nil, false
		}
		for _, it := range list {
			if m, ok := it.(map[string]any); ok {
				if id, has := m["id"]; has {
					models = append(models, pyStr(id))
				}
			}
		}
		cursor = anthropicNextCursor(data, seenCursors)
		if cursor == "" {
			break
		}
	}
	return dedupeStrings(models), true
}
