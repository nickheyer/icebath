package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Providers whose runtime is resolved before any custom provider URL lookup
var runtimeFirstProviderIDs = map[string]bool{
	"auto": true, "moa": true, "vertex": true, "google-vertex": true, "vertex-ai": true, "gcp-vertex": true, "vertexai": true,
}

const ctxLenRequirement = "must be a positive integer (e.g. 256000, not '256K')"

var resolveAuthProvider = func(ctx context.Context, name string) (string, error) {
	return provider.ResolveAuthProvider(agentPoolEnv(ctx), name)
}

// Python str of a config value with None as the empty string
func cfgText(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return anyutil.String(v)
}

// URL with its query removed and the fragment kept like urlunparse over a query free urlparse
func stripURLQuery(raw string) string {
	base, frag, _ := strings.Cut(raw, "#")
	pre, _, _ := strings.Cut(base, "?")
	if frag != "" {
		return pre + "#" + frag
	}
	return pre
}

// Normalized base URL of the enabled providers entry the configured provider names by key or by name, empty when none does
func customProviderConfiguredBaseURL(configuredProvider string, customProviders []map[string]any) string {
	wanted := normalizeCustomProviderName(configuredProvider)
	for _, entry := range customProviders {
		if customProviderRuntimeIDs(entry["provider_key"])[wanted] || customProviderRuntimeIDs(entry["name"])[wanted] {
			if u := provider.NormalizeRouteBaseURL(cfgText(entry["base_url"])); u != "" {
				return u
			}
		}
	}
	return ""
}

// Configured default route URL in effect for ctx, the custom provider URL when model.provider is not an auth provider
func configuredDefaultBaseURL(ctx context.Context, customProviders []map[string]any) string {
	configured := provider.NormalizeRouteBaseURL(settings.ModelBaseURL.Get(ctx))
	configuredProvider := strings.TrimSpace(settings.ModelProvider.Get(ctx))
	norm := normalizeCustomProviderName(configuredProvider)
	candidate := norm != ""
	if runtimeFirstProviderIDs[norm] {
		candidate = false
	} else if candidate && norm != "custom" && !strings.HasPrefix(norm, "custom:") {
		if resolved, err := resolveAuthProvider(ctx, norm); err == nil {
			candidate = strings.ToLower(strings.TrimSpace(resolved)) != norm
		}
	}
	if configured == "" && candidate {
		configured = customProviderConfiguredBaseURL(configuredProvider, customProviders)
	}
	return configured
}

// Active route URL, the requested URL when it differs from the active one only by its query
func activeRouteURL(a *AIAgent, baseURL string) string {
	active := a.baseURL
	if before, _, _ := strings.Cut(baseURL, "#"); strings.Contains(before, "?") {
		if provider.NormalizeRouteBaseURL(stripURLQuery(baseURL)) == provider.NormalizeRouteBaseURL(active) {
			active = baseURL
		}
	}
	return provider.NormalizeRouteBaseURL(active)
}

// Normalizes a model for the provider, keeping the input when the normalizer panics
func normalizeModelSafely(ctx context.Context, model, providerName string) (out string, ok bool) {
	defer func() {
		if recover() != nil {
			out, ok = model, false
		}
	}()
	return provider.NormalizeModelForProviderCtx(ctx, model, providerName), true
}

// The configured context length only when the startup runtime is the default model and route the settings of ctx name
func scopeContextLengthToDefaultRuntime(ctx context.Context, a *AIAgent, customProviders []map[string]any, configContextLength *int, baseURL string) *int {
	configuredModel := strings.TrimSpace(settings.Model.Get(ctx))
	configuredProvider := strings.TrimSpace(settings.ModelProvider.Get(ctx))
	configuredRuntimeModel, activeRuntimeModel := configuredModel, a.model
	if configuredModel != "" {
		if m, ok := normalizeModelSafely(ctx, configuredModel, a.provider); ok {
			configuredRuntimeModel = m
			if act, ok := normalizeModelSafely(ctx, a.model, a.provider); ok {
				activeRuntimeModel = act
			}
		}
	}
	configuredBaseURL := configuredDefaultBaseURL(ctx, customProviders)
	activeBaseURL := activeRouteURL(a, baseURL)
	routeMismatch := contextRouteMismatch(ctx, configuredBaseURL, activeBaseURL, configuredProvider, a.provider, true)
	modelMismatch := configuredRuntimeModel != "" && configuredRuntimeModel != activeRuntimeModel
	if modelMismatch || routeMismatch {
		shownURL := activeBaseURL
		if shownURL == "" {
			shownURL = a.provider
		}
		configuredShown := configuredBaseURL
		if configuredBaseURL == "" {
			configuredShown = configuredProvider
		}
		pin := any(nil)
		if configContextLength != nil {
			pin = *configContextLength
		}
		pkgLog().Debug(fmt.Sprintf("Ignoring model.context_length=%s for startup runtime %s at %s (configured default is %s at %s)",
			anyutil.String(pin), a.model, shownURL, configuredModel, configuredShown))
		return nil
	}
	return configContextLength
}

// Caches the context length pin on the agent and on its context engine together
func setConfigContextLength(ctx context.Context, a *AIAgent, value *int) error {
	a.configContextLength = 0
	if value != nil {
		a.configContextLength = *value
	}
	switch e := a.contextCompressor.(type) {
	case *ContextCompressor:
		if value == nil {
			e.configContextLength = nil
		} else {
			v := *value
			e.configContextLength = &v
		}
	case *hostedContextEngine:
		if value == nil {
			return e.obj.Set(ctx, "_config_context_length", nil)
		}
		return e.obj.Set(ctx, "_config_context_length", *value)
	}
	return nil
}

// Context length pin of ctx scoped to the agent's current runtime, nil when unset or for another route
func configContextLengthForRuntime(ctx context.Context, a *AIAgent) (out *int) {
	defer func() {
		if r := recover(); r != nil {
			pkgLog().Debug("Could not re-read model.context_length for the current runtime", "error", r)
			out = nil
		}
	}()
	pin, ok := settings.ModelContextLength.Lookup(ctx)
	if !ok || pin <= 0 {
		return nil
	}
	return scopeContextLengthToDefaultRuntime(ctx, a, provider.CustomProviderEntries(ctx), &pin, a.baseURL)
}

// Warns once when the matching custom provider model carries a context length that is not a positive integer
func warnInvalidCustomProviderContextLength(ctx context.Context, a *AIAgent, customProviders []map[string]any) {
	target := provider.NormalizeRouteBaseURL(a.baseURL)
	if target == "" {
		return
	}
	for _, entry := range customProviders {
		if provider.NormalizeRouteBaseURL(cfgText(entry["base_url"])) != target {
			continue
		}
		var modelCfg any
		switch models := entry["models"].(type) {
		case map[string]any:
			modelCfg = models[a.model]
			if _, present := models[a.model]; !present {
				modelCfg = map[string]any{}
			}
		case nil:
			if _, present := entry["models"]; !present {
				modelCfg = map[string]any{}
			}
		}
		var cpCtx any
		if m, ok := modelCfg.(map[string]any); ok {
			cpCtx = m["context_length"]
		}
		if cpCtx != nil && configPositiveInt(cpCtx, false) == nil {
			warnInvalidConfigInt(ctx, fmt.Sprintf("context_length for model %s in providers", errchain.Show(a.model)),
				cpCtx, ctxLenRequirement, "auto-detection", "auto-detected context window", a)
		}
		return
	}
}

// Resolves the configured, custom provider and LM Studio context lengths and the custom provider list in effect for ctx
func resolveContextLength(ctx context.Context, a *AIAgent, baseURL string) (*int, []map[string]any, int) {
	a.auxCompressionContextLengthConfig = nil
	if n, ok := settings.AuxiliaryCompressionContextLength.Lookup(ctx); ok {
		a.auxCompressionContextLengthConfig = &n
	}
	var configContextLength *int
	if n, ok := settings.ModelContextLength.Lookup(ctx); ok {
		configContextLength = &n
	}
	customProviders := provider.CustomProviderEntries(ctx)
	if configContextLength != nil {
		configContextLength = scopeContextLengthToDefaultRuntime(ctx, a, customProviders, configContextLength, baseURL)
	}
	a.customProviderEntries = customProviders
	a.customProviders = make([]any, len(customProviders))
	for i, entry := range customProviders {
		a.customProviders[i] = entry
	}
	a.mergeCustomProviderExtraBody(customProviders)
	if configContextLength == nil && len(customProviders) > 0 {
		if n := provider.CustomProviderContextLength(a.model, a.baseURL, customProviders); n != 0 {
			configContextLength = &n
		}
		if configContextLength == nil {
			warnInvalidCustomProviderContextLength(ctx, a, customProviders)
		}
	}
	a.configContextLength = 0
	var pin any
	if configContextLength != nil {
		a.configContextLength = *configContextLength
		pin = *configContextLength
		warnOnceOnPinDisagreement(ctx, a.model, a.baseURL, pin)
	}
	runtime := a.ensureLMStudioRuntimeLoaded(ctx, configContextLength)
	if lmstudioLoadWasUnverified(runtime) {
		pkgLog().Warn("LM Studio model activation was rejected or completed without a verifiable active context length; falling back to configured context")
	}
	return configContextLength, customProviders, effectiveLMStudioContextLength(pin, runtime)
}
