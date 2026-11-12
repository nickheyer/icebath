package agent

import (
	"context"
	"fmt"
	"maps"
	"math/big"
	"os"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Snapshot fields of the init and switch builders and the restore reader
type agentInitState struct {
	anthropicTokenSource        *provider.TaggedTokenSource
	bedrockRegion               string
	bedrockGuardrailConfig      map[string]any
	emptyGuardEnabled           *bool
	emptyGuardCostThresholdUSD  *guardDecimal
	emptyAttemptHistory         []emptyAttempt
	emptyStreakCostUSD          *big.Rat
	credentialPoolEntryID       string
	primaryRuntime              map[string]any
	customProviderEntries       []map[string]any
	requestAnthropicClientCache map[string]any
	servedModel                 *servedModelState
	oauth1MBetaDisabled         bool
}

func (a *AIAgent) CredentialPoolEntryID() string       { return a.credentialPoolEntryID }
func (a *AIAgent) PrimaryRuntime() map[string]any      { return a.primaryRuntime }
func (a *AIAgent) SetPrimaryRuntime(rt map[string]any) { a.primaryRuntime = rt }

// Owning custom entry of the route resolved against the config the route runs on, nil for none
func runtimeEntryOwner(ctx context.Context, a *AIAgent) any {
	if owner := provider.RuntimeEntryOwner(a.provider, a.requestedProvider, a.baseURL, provider.LoadConfigSafe(ctx, home.Dir(ctx))); owner != nil {
		return owner
	}
	return nil
}

func (a *AIAgent) reasoningEchoValue() any {
	if a.reasoningEchoFlag == nil {
		return false
	}
	return a.reasoningEchoFlag
}

// Compressor attribute, the agent value when the engine lacks it
func engineAttrOr(cc ContextEngine, name string, fallback any) any {
	if v, ok := engineAttr(cc, name); ok {
		return v
	}
	return fallback
}

func copyAnyMap(m map[string]any) map[string]any {
	out := map[string]any{}
	maps.Copy(out, m)
	return out
}

// Records the route a fallback turn restores on the next turn, with the engine state a fallback overwrites
func snapshotPrimaryRuntime(ctx context.Context, a *AIAgent) {
	cc := a.contextCompressor
	rt := map[string]any{
		"model": a.model, "provider": a.provider, "requested_provider": a.requestedProvider,
		provider.SnapshotOwnerKey: runtimeEntryOwner(ctx, a),
		"base_url":                a.baseURL, "api_mode": a.apiMode, "api_key": a.apiKey,
		"request_overrides": copyAnyMap(a.requestOverrides), "client_kwargs": copyAnyMap(a.clientKwargs),
		"use_prompt_caching": a.usePromptCaching, "use_native_cache_layout": a.useNativeCacheLayout,
		"reasoning_echo_flag":         a.reasoningEchoValue(),
		"compressor_model":            engineAttrOr(cc, "model", a.model),
		"compressor_base_url":         engineAttrOr(cc, "base_url", a.baseURL),
		"compressor_api_key":          engineAttrOr(cc, "api_key", ""),
		"compressor_provider":         engineAttrOr(cc, "provider", a.provider),
		"compressor_context_length":   engineContextLength(cc),
		"compressor_threshold_tokens": engineThresholdTokens(cc),
	}
	if a.apiMode == "anthropic_messages" {
		rt["anthropic_api_key"], rt["anthropic_base_url"], rt["is_anthropic_oauth"] = a.anthropicKeyValue(), a.anthropicBaseURL, a.isAnthropicOAuth
	}
	a.primaryRuntime = rt
}

// Primary runtime record that persists a deliberate model switch across turns
func buildPrimaryRuntimeSnapshot(ctx context.Context, a *AIAgent, apiMode string) map[string]any {
	cc := a.contextCompressor
	var reasoning any
	if len(a.reasoningConfig) > 0 {
		reasoning = copyAnyMap(a.reasoningConfig)
	}
	caps := map[string]any{}
	for k, v := range a.runtimeCapabilities {
		caps[k] = v
	}
	contextLength, thresholdTokens := 0, 0
	if cc != nil {
		contextLength, thresholdTokens = engineContextLength(cc), engineThresholdTokens(cc)
	}
	rt := map[string]any{
		"model": a.model, "provider": a.provider, "requested_provider": a.requestedProvider,
		provider.SnapshotOwnerKey: runtimeEntryOwner(ctx, a),
		"base_url":                a.baseURL, "api_mode": a.apiMode, "api_key": a.apiKey,
		"client_kwargs":      copyAnyMap(a.clientKwargs),
		"use_prompt_caching": a.usePromptCaching, "use_native_cache_layout": a.useNativeCacheLayout,
		"reasoning_config": reasoning, "reasoning_echo_flag": a.reasoningEchoValue(),
		"request_overrides": copyAnyMap(a.requestOverrides), "runtime_capabilities": caps,
		"compressor_model":            engineAttrOr(cc, "model", a.model),
		"compressor_base_url":         engineAttrOr(cc, "base_url", a.baseURL),
		"compressor_api_key":          engineAttrOr(cc, "api_key", ""),
		"compressor_provider":         engineAttrOr(cc, "provider", a.provider),
		"compressor_context_length":   contextLength,
		"compressor_api_mode":         engineAttrOr(cc, "api_mode", a.apiMode),
		"compressor_threshold_tokens": thresholdTokens,
	}
	if apiMode == "anthropic_messages" {
		rt["anthropic_api_key"], rt["anthropic_base_url"], rt["is_anthropic_oauth"] = a.anthropicKeyValue(), a.anthropicBaseURL, a.isAnthropicOAuth
	}
	return rt
}

// Readonly config the reasoning echo flag reads, the load_config_readonly reasoning_params imports
var reasoningEchoConfigLoader = func(ctx context.Context) (config.Tree, error) {
	return config.LoadReadonly(ctx, home.Dir(ctx), os.LookupEnv)
}

// Configured model.reasoning_echo, false with a class only warning when the config cannot be read
func readReasoningEchoFromConfig(ctx context.Context) bool {
	warn := func(class string) bool {
		runAgentLog().Warn(fmt.Sprintf("Agent configuration for model.reasoning_echo could not be loaded (%s); "+
			"using the default reasoning echo setting. Check the active Hermes Python "+
			"environment and dependencies; run 'icebath config check' after "+
			"configuration imports work again.", class))
		return false
	}
	cfg, err := reasoningEchoConfigLoader(ctx)
	if err != nil {
		return warn(pyErrorClass(err))
	}
	model := cfg["model"]
	if !pyrepr.Truthy(model) {
		return false
	}
	switch m := model.(type) {
	case map[string]any:
		return pyrepr.Truthy(m["reasoning_echo"])
	case config.Tree:
		return pyrepr.Truthy(m["reasoning_echo"])
	}
	return warn("AttributeError")
}

// Anthropic credential as the agent holds it, the per request token source when the route installed one
func (a *AIAgent) anthropicKeyValue() any {
	if a.anthropicTokenSource != nil {
		return a.anthropicTokenSource
	}
	return a.anthropicAPIKey
}
