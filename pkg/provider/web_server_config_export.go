package provider

import "context"

// Main slot pair as persisted after declared provider resolution and model normalization
func NormalizeMainModelAssignment(ctx context.Context, provider, model string) (string, string) {
	return normalizeMainModelAssignment(ctx, provider, model)
}

// Main slot pick routed through the model switch, a rejection being a 400 HTTPException
func ValidatedMainModelSelection(ctx context.Context, cfg map[string]any, provider, model, baseURL, apiKey string, explicitEndpoint *bool) (ModelSwitchResult, error) {
	return validatedMainModelSelection(ctx, cfg, provider, model, baseURL, apiKey, explicitEndpoint)
}

// Model block carrying a validated main slot selection
func ApplyMainModelAssignment(ctx context.Context, modelCfg any, result ModelSwitchResult, apiKey string) (map[string]any, error) {
	return applyMainModelAssignment(ctx, modelCfg, result, apiKey)
}

// Plugin registered auxiliary tasks of the home in ctx outside the built in slots
func PluginAuxTasks(ctx context.Context) []map[string]any { return pluginAuxTasks(ctx) }

// Every auxiliary slot the dashboard reads or assigns, built ins first
func AuxTaskSlots(ctx context.Context) []string { return auxTaskSlots(ctx) }

// Providers block of a legacy custom_providers row under a provider key, nil when the row converts to none
func CustomProviderEntryToProviderConfig(entry any, providerKey string) map[string]any {
	return customProviderEntryToProviderConfig(entry, providerKey)
}
