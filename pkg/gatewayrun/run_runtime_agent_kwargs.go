package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Agent constructor kwargs shared by every runtime provider resolution, request overrides passed through as resolved
func runtimeAgentKwargs(rt *provider.Runtime) map[string]any {
	var pool any
	if rt.CredentialPool != nil {
		pool = rt.CredentialPool
	}
	var overrides any
	if rt.RequestOverrides != nil {
		overrides = rt.RequestOverrides
	}
	return map[string]any{
		"api_key": rt.APIKeyValue(), "base_url": rt.BaseURL, "provider": rt.Provider,
		"requested_provider": rt.RequestedProvider, "api_mode": string(rt.APIMode),
		"command": rt.Command, "args": slices.Clone(rt.Args), "credential_pool": pool,
		"request_overrides": overrides,
	}
}

// Runtime credentials of one provider for the model the override will send
func resolveRuntimeAgentKwargsForProvider(ctx context.Context, providerName, targetModel string) (map[string]any, error) {
	rt, err := provider.ResolveRuntimeProvider(ctx, home.Dir(ctx), providerName, targetModel, "", "")
	if err != nil {
		return nil, errors.New(provider.FormatRuntimeProviderError(err))
	}
	kwargs := runtimeAgentKwargs(rt)
	overrides := maps.Clone(rt.RequestOverrides)
	if overrides == nil {
		overrides = map[string]any{}
	}
	kwargs["request_overrides"] = overrides
	capabilities := map[string]any{}
	for k, v := range rt.Capabilities {
		capabilities[k] = v
	}
	kwargs["capabilities"] = capabilities
	return kwargs, nil
}

// Live credential pool of a provider id, nil when blank or unresolvable
func credentialPoolForProvider(ctx context.Context, providerName string) any {
	name := strings.TrimSpace(providerName)
	if name == "" {
		return nil
	}
	kwargs, err := resolveRuntimeAgentKwargsForProvider(ctx, name, "")
	if err != nil {
		runLog().Debug(fmt.Sprintf("Failed to resolve credential pool for provider=%s", providerName), "error", err)
		return nil
	}
	return kwargs["credential_pool"]
}

// Provider credentials for gateway created agents, a fallback entry's model and switch notice carried when the primary failed
func resolveRuntimeAgentKwargs(ctx context.Context) (map[string]any, error) {
	h := home.Dir(ctx)
	modelCfg := provider.PrimaryModelConfig(ctx, h)
	text := func(key string) string {
		s, _ := modelCfg[key].(string)
		return strings.TrimSpace(s)
	}
	primaryModel, primaryProvider := text("default"), text("provider")
	rt, fallbackEntry, err := provider.ResolveRuntimeWithFallback(ctx, h, "", "", "", "")
	if err != nil {
		return nil, errors.New(provider.FormatRuntimeProviderError(err))
	}
	kwargs := runtimeAgentKwargs(rt)
	if fallbackEntry != nil {
		fallbackProvider := rt.Provider
		if fallbackProvider == "" {
			fallbackProvider, _ = fallbackEntry["provider"].(string)
		}
		if fallbackProvider == "" {
			fallbackProvider = "unknown"
		}
		fallbackModel, _ := fallbackEntry["model"].(string)
		noticeModel := fallbackModel
		if noticeModel == "" {
			noticeModel = "default"
		}
		kwargs["model"] = fallbackEntry["model"]
		kwargs["_fallback_notice"] = provider.PreAgentFallbackNotice(primaryProvider, primaryModel, fallbackProvider, noticeModel)
		return kwargs, nil
	}
	capabilities := map[string]any{}
	for k, v := range rt.Capabilities {
		capabilities[k] = v
	}
	kwargs["capabilities"] = capabilities
	return kwargs, nil
}

// Request override dicts merged with nested dicts deep merged, either side alone returned as a copy
func deepMergeRequestOverrides(base, override map[string]any) map[string]any {
	baseDict, overrideDict := maps.Clone(base), maps.Clone(override)
	if baseDict == nil {
		baseDict = map[string]any{}
	}
	if overrideDict == nil {
		overrideDict = map[string]any{}
	}
	if len(baseDict) == 0 {
		return overrideDict
	}
	if len(overrideDict) == 0 {
		return baseDict
	}
	return config.DeepMerge(baseDict, overrideDict)
}
