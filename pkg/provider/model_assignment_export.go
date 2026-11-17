package provider

import "context"

// Main slot pair as persisted after declared provider resolution and model normalization
func NormalizeMainModelAssignment(ctx context.Context, provider, model string) (string, string) {
	return normalizeMainModelAssignment(ctx, provider, model)
}

// Main slot pick routed through the model switch, a rejection being a 400 refusal
func ValidatedMainModelSelection(ctx context.Context, cfg map[string]any, provider, model, baseURL, apiKey string, explicitEndpoint *bool) (ModelSwitchResult, error) {
	return validatedMainModelSelection(ctx, cfg, provider, model, baseURL, apiKey, explicitEndpoint)
}

// Model block carrying a validated main slot selection
func ApplyMainModelAssignment(ctx context.Context, modelCfg any, result ModelSwitchResult, apiKey string) (map[string]any, error) {
	return applyMainModelAssignment(ctx, modelCfg, result, apiKey)
}
