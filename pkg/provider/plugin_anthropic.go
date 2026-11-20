package provider

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
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
	headers := pyhttpx.OrderedHeaderMap(pyhttpx.HeaderLine{Name: "x-api-key", Value: apiKey}, pyhttpx.HeaderLine{Name: "anthropic-version", Value: "2023-06-01"}, pyhttpx.HeaderLine{Name: "Accept", Value: "application/json"})
	var models []string
	seenCursors := map[string]bool{}
	cursor := ""
	for range anthropicModelsMaxPages {
		data, err := modelsGetJSON(ctx, anthropicModelsURL(baseURL, cursor), timeout, headers, nil)
		if err != nil {
			pkgLog().Debug("fetch_models(anthropic): " + err.Error())
			return nil, false
		}
		doc, isDict := data.(map[string]any)
		if !isDict {
			pkgLog().Debug("fetch_models(anthropic): the models reply is " + errchain.Kind(data) + ", expected a mapping")
			return nil, false
		}
		items, present := doc["data"]
		if !present {
			items = []any{}
		}
		list, isList := items.([]any)
		if !isList {
			pkgLog().Debug("fetch_models(anthropic): the models list is " + errchain.Kind(items) + ", expected a list")
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
