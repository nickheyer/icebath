package provider

import "context"

// ResetBedrockClientCache drops every cached client, profile resolution and fallback warning
func ResetBedrockClientCache() {
	bedrockClientsMu.Lock()
	for k, c := range bedrockClients {
		c.closeIdle()
		delete(bedrockClients, k)
	}
	bedrockClientsMu.Unlock()
	bedrockProfileMu.Lock()
	bedrockProfileCache = map[string]string{}
	bedrockProfileMu.Unlock()
	bedrockWarnedMu.Lock()
	bedrockWarnedDefault = map[string]bool{}
	bedrockWarnedMu.Unlock()
}

// ResetBedrockDiscoveryCache clears the model discovery cache
func ResetBedrockDiscoveryCache() {
	bedrockDiscoveryMu.Lock()
	bedrockDiscoveryCache = map[string]bedrockDiscoveryEntry{}
	bedrockDiscoveryMu.Unlock()
}

// BedrockContextLength is the window of a model, probing the region first when probe is set
func BedrockContextLength(ctx context.Context, model, region string, probe bool) int {
	return bedrockContextLength(ctx, model, region, probe)
}
