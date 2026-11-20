package gatewayrun

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Effective gateway model route and context window
type gatewayModelContext struct {
	model, provider, baseURL string
	contextLength            int
	contextSource            string
}

// Configured gateway route and its context window, a session route's endpoint replacing the default credentials
func resolveGatewayModelContext(ctx context.Context, model string, route map[string]any) gatewayModelContext {
	resolvedModel := model
	if resolvedModel == "" {
		resolvedModel = gateway.ResolveGatewayModel(ctx, nil)
	}
	var configContextLength *int
	var providerName, baseURL, apiKey, configuredModel, configuredProvider, configuredBaseURL string
	var customProviders []map[string]any
	if data := gateway.LoadGatewayConfigMap(ctx, ""); len(data) > 0 {
		if modelCfg, ok := data["model"].(map[string]any); ok {
			configuredModel = noneText(modelCfg["default"])
			if !pyrepr.Truthy(modelCfg["default"]) {
				configuredModel = noneText(modelCfg["model"])
			}
			if raw, present := modelCfg["context_length"]; present && raw != nil {
				if n, err := pyint.Int64(raw); err == nil {
					value := int(n)
					configContextLength = &value
				}
			}
			if pyrepr.Truthy(modelCfg["provider"]) {
				configuredProvider = noneText(modelCfg["provider"])
			}
			providerName = configuredProvider
			if pyrepr.Truthy(modelCfg["base_url"]) {
				configuredBaseURL = noneText(modelCfg["base_url"])
			}
			baseURL = configuredBaseURL
		}
		customProviders = provider.CompatibleCustomProviders(data)
	}
	if route != nil && pyrepr.Truthy(route["base_url"]) {
		if pyrepr.Truthy(route["provider"]) {
			providerName = noneText(route["provider"])
		}
		baseURL = noneText(route["base_url"])
		apiKey = noneText(route["api_key"])
	} else if runtime, err := resolveRuntimeAgentKwargs(ctx); err == nil {
		if pyrepr.Truthy(runtime["provider"]) {
			providerName = noneText(runtime["provider"])
		}
		if pyrepr.Truthy(runtime["base_url"]) {
			baseURL = noneText(runtime["base_url"])
		}
		apiKey = noneText(runtime["api_key"])
	}
	if configContextLength != nil && provider.ShouldClearContextPin(configuredModel, resolvedModel, configuredBaseURL, baseURL, configuredProvider, providerName) {
		configContextLength = nil
	}
	if configContextLength == nil && len(customProviders) > 0 && baseURL != "" {
		if n := provider.CustomProviderContextLength(resolvedModel, baseURL, customProviders, nil); n != 0 {
			configContextLength = &n
		}
	}
	pinned := 0
	if configContextLength != nil {
		pinned = *configContextLength
	}
	contextLength := provider.GetModelContextLength(ctx, provider.ContextLengthRequest{Model: resolvedModel, BaseURL: baseURL,
		APIKey: apiKey, Provider: providerName, ConfigContextLength: pinned, CustomProviders: customProviders})
	_, known := provider.LongestDefaultContextMatch(strings.ToLower(resolvedModel))
	source := "detected"
	switch {
	case configContextLength != nil:
		source = "config"
	case contextLength == provider.DefaultFallbackContext && !known:
		source = "default"
	}
	return gatewayModelContext{model: resolvedModel, provider: providerName, baseURL: baseURL, contextLength: contextLength, contextSource: source}
}
