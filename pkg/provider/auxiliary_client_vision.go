package provider

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Provider client resolution seam the vision and cache paths call, swapped by tests as hermes patches the module name
var auxResolveProviderClientFn = ResolveProviderClient

// Aggregators tried in order once the main provider cannot serve vision
var auxVisionAutoProviderOrder = []string{"openrouter", "nous", "deepinfra"}

// Vision verdict of the main provider and model, unknown capability reading as true so the call is attempted
var auxMainModelSupportsVision = func(ctx context.Context, provider, model string) bool {
	var cfg map[string]any
	var loadErr error
	if err := auxRecoverRaise(func() { cfg, loadErr = auxLoadConfigReadonlyFn(ctx) }); err != nil || loadErr != nil {
		return true
	}
	env := &tools.Env{Home: home.Dir(ctx), Config: cfg, Values: &sync.Map{}}
	if runtime := auxRuntimeMain(ctx); runtime != nil {
		env.Values.Store("runtime_main", runtime)
	}
	supported, known := false, false
	if err := auxRecoverRaise(func() { supported, known = tools.LookupSupportsVision(ctx, env, provider, model) }); err != nil {
		return true
	}
	return !known || supported
}

// Vision provider name with the auxiliary aliases applied
func auxNormalizeVisionProvider(ctx context.Context, provider string) string {
	return auxNormalizeAuxProvider(ctx, provider)
}

// DeepInfra vision client over the live catalog default model, nil when the catalog names none
func auxDeepinfraStrictVisionBackend(ctx context.Context, model string) (AuxClient, string, error) {
	visionModel := model
	if visionModel == "" {
		visionModel = auxResolveProviderVisionDefault(ctx, "deepinfra")
	}
	if visionModel == "" {
		slog.Debug("Vision auto-detect: deepinfra catalog unreachable or returned no vision-tagged models — skipping")
		return nil, "", nil
	}
	return auxResolveProviderClientFn(ctx, "deepinfra", visionModel, ProviderClientOptions{IsVision: true})
}

// Explicitly requested vision backends by normalized provider name
var auxStrictVisionBackends = map[string]func(ctx context.Context, model string) (AuxClient, string, error){
	"openrouter": func(ctx context.Context, model string) (AuxClient, string, error) {
		return auxTryOpenrouter(ctx, nil, model, "")
	},
	"nous": func(ctx context.Context, model string) (AuxClient, string, error) {
		return auxResolveProviderClientFn(ctx, "nous", model, ProviderClientOptions{IsVision: true})
	},
	"openai-codex": func(ctx context.Context, model string) (AuxClient, string, error) {
		return auxResolveProviderClientFn(ctx, "openai-codex", model, ProviderClientOptions{IsVision: true})
	},
	"anthropic": func(ctx context.Context, model string) (AuxClient, string, error) {
		return auxTryAnthropic(ctx, nil, "")
	},
	"deepinfra": auxDeepinfraStrictVisionBackend,
	"custom": func(ctx context.Context, model string) (AuxClient, string, error) {
		return auxTryCustomEndpoint(ctx)
	},
}

// Client of a strict vision backend, nil when the provider has none
func auxResolveStrictVisionBackend(ctx context.Context, provider, model string) (AuxClient, string, error) {
	backend, ok := auxStrictVisionBackends[auxNormalizeVisionProvider(ctx, provider)]
	if !ok {
		return nil, "", nil
	}
	return backend(ctx, model)
}

// GetAvailableVisionBackends lists usable vision backends in auto order, the active provider first
func GetAvailableVisionBackends(ctx context.Context) ([]string, error) {
	var available []string
	mainProvider := auxReadMainProvider(ctx)
	if mainProvider != "" && mainProvider != "auto" {
		var client AuxClient
		var err error
		if slices.Contains(auxVisionAutoProviderOrder, mainProvider) {
			client, _, err = auxResolveStrictVisionBackendFn(ctx, mainProvider, "")
		} else {
			client, _, err = auxResolveProviderClientFn(ctx, mainProvider, auxReadMainModel(ctx), ProviderClientOptions{})
		}
		if err != nil {
			return nil, err
		}
		if client != nil {
			available = append(available, mainProvider)
		}
	}
	for _, p := range auxVisionAutoProviderOrder {
		if slices.Contains(available, p) {
			continue
		}
		client, _, err := auxResolveStrictVisionBackendFn(ctx, p, "")
		if err != nil {
			return nil, err
		}
		if client != nil {
			available = append(available, p)
		}
	}
	return available, nil
}

// Applies the explicit model override and the async wrap to a resolved vision client
func auxFinalizeVisionClient(ctx context.Context, resolvedProvider string, syncClient AuxClient, defaultModel, resolvedModel string, asyncMode bool) (string, AuxClient, string) {
	if syncClient == nil {
		return resolvedProvider, nil, ""
	}
	finalModel := resolvedModel
	if finalModel == "" {
		finalModel = defaultModel
	}
	if asyncMode {
		asyncClient, asyncModel := auxToAsyncClient(ctx, syncClient, finalModel, true)
		return resolvedProvider, asyncClient, asyncModel
	}
	return resolvedProvider, syncClient, finalModel
}

// Main provider vision client for auto detection, nil falling through to the aggregator chain
func auxVisionMainProviderClient(ctx context.Context, mainProvider, mainModel string, runtime map[string]any, resolvedModel, resolvedAPIMode string) (AuxClient, string, error) {
	providerVisionDefault := auxResolveProviderVisionDefault(ctx, mainProvider)
	visionModel := providerVisionDefault
	if visionModel == "" {
		visionModel = mainModel
	}
	if mainProvider == "nous" {
		pick := resolvedModel
		if pick == "" {
			pick = providerVisionDefault
		}
		syncClient, defaultModel, err := auxResolveStrictVisionBackendFn(ctx, mainProvider, pick)
		if err != nil || syncClient == nil {
			return nil, "", err
		}
		slog.Info(fmt.Sprintf("Vision auto-detect: using main provider %s (%s)", mainProvider, auxFirstNonEmpty(defaultModel, resolvedModel, mainModel)))
		return syncClient, defaultModel, nil
	}
	if auxProvidersWithoutVision[mainProvider] {
		slog.Debug(fmt.Sprintf("Vision auto-detect: skipping main provider %s (no vision support) — falling through to aggregator chain", mainProvider))
		return nil, "", nil
	}
	if !auxMainModelSupportsVision(ctx, mainProvider, visionModel) {
		slog.Debug(fmt.Sprintf("Vision auto-detect: skipping main provider %s (reports no vision capability) — falling through to aggregator chain", mainProvider))
		return nil, "", nil
	}
	var rpcBaseURL string
	var rpcAPIKey any
	rpcAPIMode := resolvedAPIMode
	if mainProvider == "custom" || strings.HasPrefix(mainProvider, "custom:") {
		var customBase, customMode string
		var customKey any
		if base, _ := runtime["base_url"].(string); base != "" {
			customBase, customMode = base, cfgString(runtime["api_mode"])
			if auxTruthy(runtime["api_key"]) {
				customKey = runtime["api_key"]
			}
		} else {
			var key any
			var err error
			customBase, key, customMode, err = auxResolveCustomRuntimeFn(ctx)
			if err != nil {
				return nil, "", err
			}
			if auxTruthyKey(key) {
				customKey = key
			}
		}
		if customBase != "" {
			rpcBaseURL, rpcAPIKey = customBase, customKey
			rpcAPIMode = auxFirstNonEmpty(resolvedAPIMode, customMode)
		}
	}
	rpcClient, rpcModel, err := auxResolveProviderClientFn(ctx, mainProvider, visionModel, ProviderClientOptions{
		APIMode: rpcAPIMode, ExplicitBaseURL: rpcBaseURL, ExplicitAPIKey: rpcAPIKey, MainRuntime: runtime, IsVision: true,
	})
	if err != nil || rpcClient == nil {
		return nil, "", err
	}
	slog.Info(fmt.Sprintf("Vision auto-detect: using main provider %s (%s)", mainProvider, auxFirstNonEmpty(rpcModel, visionModel)))
	return rpcClient, auxFirstNonEmpty(rpcModel, visionModel), nil
}

// Auto vision route, the main provider first and then OpenRouter, Nous and DeepInfra
func auxVisionAutoRoute(ctx context.Context, runtime map[string]any, resolvedModel, resolvedAPIMode string, asyncMode bool) (string, AuxClient, string, error) {
	mainProvider := auxPyStrOr(runtime["provider"], auxReadMainProvider(ctx))
	mainModel := auxPyStrOr(runtime["model"], auxReadMainModel(ctx))
	if strings.ToLower(strings.TrimSpace(mainProvider)) == "moa" {
		if aggProvider, aggModel := auxResolveMoaAggregator(ctx, mainModel); aggProvider != "" && aggModel != "" {
			mainProvider, mainModel = aggProvider, aggModel
			runtime = auxCloneMap(runtime)
			runtime["base_url"], runtime["api_key"], runtime["api_mode"] = "", "", ""
		}
	}
	if mainProvider != "" && mainProvider != "auto" && mainProvider != "moa" {
		client, defaultModel, err := auxVisionMainProviderClient(ctx, mainProvider, mainModel, runtime, resolvedModel, resolvedAPIMode)
		if err != nil {
			return "", nil, "", err
		}
		if client != nil {
			p, c, m := auxFinalizeVisionClient(ctx, mainProvider, client, defaultModel, resolvedModel, asyncMode)
			return p, c, m, nil
		}
	}
	for _, candidate := range auxVisionAutoProviderOrder {
		if candidate == mainProvider {
			continue
		}
		syncClient, defaultModel, err := auxResolveStrictVisionBackendFn(ctx, candidate, "")
		if err != nil {
			return "", nil, "", err
		}
		if syncClient != nil {
			p, c, m := auxFinalizeVisionClient(ctx, candidate, syncClient, defaultModel, resolvedModel, asyncMode)
			return p, c, m, nil
		}
	}
	slog.Debug("Auxiliary vision client: none available")
	return "", nil, "", nil
}

// ZAI vision endpoints on the OpenAI compatible wire since the Anthropic wire rejects multimodal max_tokens
var auxZaiOpenAIVisionURLs = []string{"https://open.bigmodel.cn/api/paas/v4", "https://api.z.ai/api/paas/v4"}

// ResolveVisionProviderClient resolves the provider, client and model vision tasks use, empty provider and model for None
func ResolveVisionProviderClient(ctx context.Context, provider, model, baseURL string, apiKey any, asyncMode bool, mainRuntime map[string]any) (string, AuxClient, string, error) {
	runtime := auxNormalizeMainRuntime(ctx, mainRuntime)
	route := auxResolveTaskProviderModelFn(ctx, "vision", provider, model, baseURL, apiKey)
	rawRequested := route.Provider
	requested := auxNormalizeVisionProvider(ctx, rawRequested)
	if route.BaseURL != "" {
		forBase := "custom"
		if requested != "" && requested != "auto" {
			forBase = requested
		}
		client, finalModel, err := auxResolveProviderClientFn(ctx, forBase, route.Model, ProviderClientOptions{
			AsyncMode: asyncMode, ExplicitBaseURL: route.BaseURL, ExplicitAPIKey: route.APIKey, APIMode: route.APIMode, MainRuntime: runtime,
		})
		if err != nil {
			return "", nil, "", err
		}
		if client == nil {
			finalModel = ""
		}
		return forBase, client, finalModel, nil
	}
	if requested == "auto" {
		return auxVisionAutoRoute(ctx, runtime, route.Model, route.APIMode, asyncMode)
	}
	if slices.Contains(auxVisionAutoProviderOrder, requested) {
		syncClient, defaultModel, err := auxResolveStrictVisionBackendFn(ctx, requested, route.Model)
		if err != nil {
			return "", nil, "", err
		}
		p, c, m := auxFinalizeVisionClient(ctx, requested, syncClient, defaultModel, route.Model, asyncMode)
		return p, c, m, nil
	}
	if requested == "zai" {
		var zaiKey any
		if auxTruthy(route.APIKey) {
			zaiKey = route.APIKey
		}
		for _, zaiURL := range auxZaiOpenAIVisionURLs {
			client, finalModel, err := auxGetCachedClientFn(ctx, auxCachedClientRequest{
				Provider: requested, Model: route.Model, AsyncMode: asyncMode, BaseURL: zaiURL, APIKey: zaiKey,
				APIMode: "chat_completions", MainRuntime: runtime, IsVision: true,
			})
			if err != nil {
				return "", nil, "", err
			}
			if client != nil {
				p, c, m := auxFinalizeVisionClient(ctx, requested, client, finalModel, route.Model, asyncMode)
				return p, c, m, nil
			}
		}
	}
	client, finalModel, err := auxGetCachedClientFn(ctx, auxCachedClientRequest{
		Provider: rawRequested, Model: route.Model, AsyncMode: asyncMode, APIMode: route.APIMode, MainRuntime: runtime, IsVision: true,
	})
	if err != nil {
		return "", nil, "", err
	}
	if client == nil {
		finalModel = ""
	}
	return requested, client, finalModel, nil
}

// First non empty string, empty when all are
func auxFirstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Python str of a truthy value, the fallback when the value is falsy
func auxPyStrOr(v any, fallback string) string {
	if auxTruthy(v) {
		return fmt.Sprint(v)
	}
	return fallback
}

// Strict vision backend seam the vision auto route tests swap
var auxResolveStrictVisionBackendFn = auxResolveStrictVisionBackend
