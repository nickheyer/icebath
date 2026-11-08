package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Providers whose runtime is resolved before any custom provider URL lookup
var runtimeFirstProviderIDs = map[string]bool{
	"auto": true, "moa": true, "vertex": true, "google-vertex": true, "vertex-ai": true, "gcp-vertex": true, "vertexai": true,
}

const ctxLenRequirement = "must be a positive integer (e.g. 256000, not '256K')"

var (
	resolveAuthProvider = func(ctx context.Context, name string) (string, error) {
		return provider.ResolveAuthProvider(agentPoolEnv(ctx), name)
	}
	contextLengthConfigLoader = func(ctx context.Context) map[string]any { return provider.LoadConfigSafe(home.Dir(ctx)) }
)

// Python str of a config value with None as the empty string
func cfgText(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}

// First truthy value of the candidates, nil when every one is falsy
func firstTruthy(values ...any) any {
	for _, v := range values {
		if pyTruthy(v) {
			return v
		}
	}
	return nil
}

// Python int() of a config value, false on the TypeError or ValueError
func configIntOf(v any) (int, bool) {
	n, err := pyint.Of(v)
	if err != nil || !n.IsInt64() {
		return 0, false
	}
	return int(n.Int64()), true
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

// Normalized base URL of the configured custom provider, disabled providers masking their custom twin
func customProviderConfiguredBaseURL(configuredProvider string, agentCfg map[string]any, customProviders []map[string]any) string {
	wanted := normalizeCustomProviderName(configuredProvider)
	disabled := map[string]bool{}
	if userProviders, ok := agentCfg["providers"].(map[string]any); ok {
		for key, raw := range userProviders {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			ids := customProviderRuntimeIDs(key)
			for id := range customProviderRuntimeIDs(entry["name"]) {
				ids[id] = true
			}
			if !provider.IsProviderEnabled(entry) {
				for id := range ids {
					disabled[id] = true
				}
				continue
			}
			if ids[wanted] {
				if u := provider.NormalizeRouteBaseURL(cfgText(firstTruthy(entry["api"], entry["url"], entry["base_url"]))); u != "" {
					return u
				}
			}
		}
	}
	for _, entry := range customProviders {
		keyIDs := customProviderRuntimeIDs(entry["provider_key"])
		masked := false
		for id := range keyIDs {
			masked = masked || disabled[id]
		}
		if masked {
			continue
		}
		if keyIDs[wanted] || customProviderRuntimeIDs(entry["name"])[wanted] {
			if u := provider.NormalizeRouteBaseURL(cfgText(entry["base_url"])); u != "" {
				return u
			}
		}
	}
	return ""
}

// Configured default route URL, the custom provider URL when model.provider is not an auth provider
func configuredDefaultBaseURL(ctx context.Context, agentCfg, modelCfg map[string]any, customProviders []map[string]any) string {
	configured := provider.NormalizeRouteBaseURL(cfgText(modelCfg["base_url"]))
	configuredProvider := strings.TrimSpace(cfgText(firstTruthy(modelCfg["provider"])))
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
		configured = customProviderConfiguredBaseURL(configuredProvider, agentCfg, customProviders)
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
func normalizeModelSafely(model, providerName string) (out string, ok bool) {
	defer func() {
		if recover() != nil {
			out, ok = model, false
		}
	}()
	return provider.NormalizeModelForProvider(model, providerName), true
}

// The configured context length only when the startup runtime is the configured default model and route
func scopeContextLengthToDefaultRuntime(ctx context.Context, a *AIAgent, agentCfg, modelCfg map[string]any, customProviders []map[string]any, configContextLength *int, baseURL string) *int {
	def := modelCfg["default"]
	if _, ok := def.(map[string]any); ok {
		m, _ := config.SplitModelDefault(def)
		def = m
	}
	configuredModel := strings.TrimSpace(cfgText(firstTruthy(def)))
	configuredRuntimeModel, activeRuntimeModel := configuredModel, a.model
	if configuredModel != "" {
		if m, ok := normalizeModelSafely(configuredModel, a.provider); ok {
			configuredRuntimeModel = m
			if act, ok := normalizeModelSafely(a.model, a.provider); ok {
				activeRuntimeModel = act
			}
		}
	}
	configuredBaseURL := configuredDefaultBaseURL(ctx, agentCfg, modelCfg, customProviders)
	activeBaseURL := activeRouteURL(a, baseURL)
	routeMismatch := contextRouteMismatch(ctx, configuredBaseURL, activeBaseURL, strings.TrimSpace(cfgText(firstTruthy(modelCfg["provider"]))), a.provider, true)
	modelMismatch := configuredRuntimeModel != "" && configuredRuntimeModel != activeRuntimeModel
	if modelMismatch || routeMismatch {
		shownURL := activeBaseURL
		if shownURL == "" {
			shownURL = a.provider
		}
		configuredShown := any(configuredBaseURL)
		if configuredBaseURL == "" {
			configuredShown = modelCfg["provider"]
		}
		pin := any(nil)
		if configContextLength != nil {
			pin = *configContextLength
		}
		runAgentLog().Debug(fmt.Sprintf("Ignoring model.context_length=%s for startup runtime %s at %s (configured default is %s at %s)",
			pyrepr.Str(pin), a.model, shownURL, configuredModel, pyrepr.Str(configuredShown)))
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

// Re-reads model.context_length scoped to the agent's current runtime, nil when absent or for another route
func configContextLengthForRuntime(ctx context.Context, a *AIAgent, cfg map[string]any) (out *int) {
	defer func() {
		if r := recover(); r != nil {
			runAgentLog().Debug("Could not re-read model.context_length for the current runtime", "error", r)
			out = nil
		}
	}()
	agentCfg := cfg
	if agentCfg == nil {
		agentCfg = contextLengthConfigLoader(ctx)
	}
	if agentCfg == nil {
		return nil
	}
	modelSection, ok := agentCfg["model"].(map[string]any)
	if _, present := agentCfg["model"]; present && !ok {
		return nil
	}
	pinRaw := modelSection["context_length"]
	if _, isBool := pinRaw.(bool); pinRaw == nil || isBool {
		return nil
	}
	pin, ok := configIntOf(pinRaw)
	if !ok {
		runAgentLog().Debug("Could not re-read model.context_length for the current runtime")
		return nil
	}
	if pin <= 0 {
		return nil
	}
	return scopeContextLengthToDefaultRuntime(ctx, a, agentCfg, modelSection, provider.CompatibleCustomProviders(agentCfg), &pin, a.baseURL)
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
			warnInvalidConfigInt(ctx, fmt.Sprintf("context_length for model %s in custom_providers", pyrepr.Repr(a.model)),
				cpCtx, ctxLenRequirement, "auto-detection", "auto-detected context window", a)
		}
		return
	}
}

// Resolves the configured, custom provider and LM Studio context lengths and the custom provider list
func resolveContextLength(ctx context.Context, a *AIAgent, agentCfg map[string]any, baseURL string) (*int, []map[string]any, int, map[string]any) {
	var auxCfg any = map[string]any{}
	if aux, ok := agentCfg["auxiliary"].(map[string]any); ok {
		if v, present := aux["compression"]; present {
			auxCfg = v
		}
	}
	a.auxCompressionContextLengthConfig = 0
	if m, ok := auxCfg.(map[string]any); ok && m["context_length"] != nil {
		if n, ok := configIntOf(m["context_length"]); ok {
			a.auxCompressionContextLengthConfig = n
		}
	}
	modelCfg, _ := agentCfg["model"].(map[string]any)
	if _, present := agentCfg["model"]; !present {
		modelCfg = map[string]any{}
	}
	modelSection := modelCfg
	if modelSection == nil {
		modelSection = map[string]any{}
	}
	var configContextLength *int
	if raw := modelSection["context_length"]; raw != nil {
		if n, ok := configIntOf(raw); ok {
			configContextLength = &n
		} else {
			warnInvalidConfigInt(ctx, "model.context_length in config.yaml", raw,
				"must be a plain integer (e.g. 256000, not '256K')", "auto-detection", "auto-detected context window", a)
		}
	}
	customProviders := provider.CompatibleCustomProviders(agentCfg)
	if configContextLength != nil {
		configContextLength = scopeContextLengthToDefaultRuntime(ctx, a, agentCfg, modelSection, customProviders, configContextLength, baseURL)
	}
	a.customProviderEntries = customProviders
	a.customProviders = make([]any, len(customProviders))
	for i, entry := range customProviders {
		a.customProviders[i] = entry
	}
	a.mergeCustomProviderExtraBody(customProviders)
	if configContextLength == nil && len(customProviders) > 0 {
		if n := provider.CustomProviderContextLength(a.model, a.baseURL, customProviders, agentCfg); n != 0 {
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
		runAgentLog().Warn("LM Studio model activation was rejected or completed without a verifiable active context length; falling back to configured context")
	}
	return configContextLength, customProviders, effectiveLMStudioContextLength(pin, runtime), modelCfg
}
