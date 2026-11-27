package provider

import (
	"maps"
	"slices"

	"github.com/nickheyer/icebath/internal/provider"
)

// Host of NVIDIA's cloud, which bills requests to the application that sent them
const cloudHost = "integrate.api.nvidia.com"

// Tool messages without the name and tool_name fields NIM's schema refuses
func messages(_ *provider.Call, messages []map[string]any) []map[string]any {
	out := make([]map[string]any, len(messages))
	for i, m := range messages {
		out[i] = m
		_, name := m["name"]
		_, tool := m["tool_name"]
		if m["role"] == "tool" && (name || tool) {
			out[i] = maps.Clone(m)
			delete(out[i], "name")
			delete(out[i], "tool_name")
		}
	}
	return out
}

// Attribution NVIDIA's cloud bills icebath's requests under
func headers(call *provider.Call) map[string]string {
	if provider.BaseHostMatches(call.Route.BaseURL, cloudHost) {
		return map[string]string{"X-BILLING-INVOKE-ORIGIN": "IcebathAgent"}
	}
	return nil
}

// Model id with the vendor prefix it dropped put back from the curated models, when exactly one of them ends in it
func native(spec provider.Spec, model string) string {
	return provider.RepairPrefix(model, append(slices.Clone(spec.StaticModels), spec.FallbackModels...))
}
