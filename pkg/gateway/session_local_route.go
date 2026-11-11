package gateway

import (
	"context"
	"maps"
	"strings"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Launch params with an explicit model decoded as every startup path decodes it, true when a new mapping was resolved
func resolveLaunchRoute(ctx context.Context, params, cfg map[string]any) (map[string]any, bool, error) {
	model, isText := params["model"].(string)
	_, hasBase := params["base_url"]
	_, hasKey := params["api_key"]
	if !isText || strings.TrimSpace(model) == "" || hasBase || hasKey {
		return params, false, nil
	}
	modelCfg, _ := cfg["model"].(map[string]any)
	explicit := ""
	if raw := params["provider"]; pyTruthy(raw) {
		explicit = strings.TrimSpace(pyStrOf(raw))
	}
	current := explicit
	if current == "" && pyTruthy(modelCfg["provider"]) {
		current = pyStrOf(modelCfg["provider"])
	}
	userProviders, _ := cfg["providers"].(map[string]any)
	customProviders, _ := cfg["custom_providers"].([]any)
	route, err := provider.ResolveStartupModelRoute(ctx, model, provider.StartupRouteOptions{
		ExplicitProvider: explicit, CurrentProvider: strings.ToLower(strings.TrimSpace(current)),
		UserProviders: userProviders, CustomProviders: customProviders, AliasConfig: cfg})
	if err != nil {
		return nil, false, err
	}
	if route == nil {
		return params, false, nil
	}
	resolved := maps.Clone(params)
	resolved["model"] = route.Model
	if route.Provider != "" {
		resolved["provider"] = route.Provider
	}
	if route.BaseURL != "" {
		resolved["base_url"] = route.BaseURL
	}
	if route.APIKey != "" {
		resolved["api_key"] = route.APIKey
	}
	return resolved, true, nil
}
