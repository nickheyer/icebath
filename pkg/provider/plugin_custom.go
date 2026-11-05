package provider

import (
	"context"
	"strings"
	"time"
)

// Profiles the bundled custom plugin registers, any user configured local or relay endpoint
func customPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "custom"}, "ollama", "local", "vllm", "llamacpp", "llama.cpp", "llama-cpp")
	base := p
	p.SupportedReasoningEffortsHook = func(context.Context, string) ([]string, bool, error) {
		return append([]string(nil), openAICompatWireEfforts...), true, nil
	}
	p.DefaultReasoningConfigHook = func(string) *Reasoning { return &Reasoning{Enabled: bptr(true), Effort: "medium"} }
	p.BuildAPIKwargsExtrasHook = customBuildAPIKwargsExtras
	p.FetchModelsHook = func(ctx context.Context, apiKey, baseURL string, timeout time.Duration) ([]string, bool) {
		if baseURL == "" && base.BaseURL == "" {
			return nil, false
		}
		return base.FetchModels(ctx, apiKey, baseURL, timeout)
	}
	return []Profile{p}
}

// Ollama num_ctx option, reasoning off as effort none with think false on Ollama, Groq default, else the clamped effort
func customBuildAPIKwargsExtras(reasoning *Reasoning, kw map[string]any) (map[string]any, map[string]any) {
	extraBody, topLevel := map[string]any{}, map[string]any{}
	if numCtx := kw["ollama_num_ctx"]; pyTruthy(numCtx) {
		extraBody["options"] = map[string]any{"num_ctx": numCtx}
	}
	cfg := reasoningConfigMap(reasoning)
	if len(cfg) == 0 {
		return extraBody, topLevel
	}
	effort, _ := cfg["effort"].(string)
	effort = strings.ToLower(strings.TrimSpace(effort))
	baseURL := hookString(kw, "base_url")
	switch {
	case effort == "none" || cfg["enabled"] == false:
		topLevel["reasoning_effort"] = "none"
		if looksLikeOllamaEndpoint(baseURL) {
			extraBody["think"] = false
		}
	case effort != "" && baseURLHostMatches(baseURL, "api.groq.com"):
		topLevel["reasoning_effort"] = "default"
	case effort != "":
		topLevel["reasoning_effort"] = clampEffort(effort, openAICompatWireEfforts, nil)
	}
	return extraBody, topLevel
}
