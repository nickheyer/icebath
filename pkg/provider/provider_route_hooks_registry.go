package provider

import "context"

// Registry ids paired with their inference base URL in registry order, hidden rows left out
func RegistryInferenceRoutes(ctx context.Context) [][2]string {
	var out [][2]string
	for _, it := range providerRegistry().items(ctx) {
		out = append(out, [2]string{it.Key, registryInferenceBaseURL(it.Row)})
	}
	return out
}
