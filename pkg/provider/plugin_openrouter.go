package provider

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

var (
	openrouterModelsMu    sync.Mutex
	openrouterModelsCache []string
	openrouterModelsSet   bool
)

// Profiles the bundled openrouter plugin registers
func openrouterPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "openrouter", display: "OpenRouter", desc: "OpenRouter \u2014 unified API for 200+ models",
		signup: "https://openrouter.ai/keys", base: OpenRouterBaseURL, modelsURL: "https://openrouter.ai/api/v1/models",
		keys:      []string{"OPENROUTER_API_KEY"},
		fallbacks: []string{"anthropic/claude-sonnet-4.6", "openai/gpt-5.4", "deepseek/deepseek-chat", "google/gemini-3.8-flash", "google/gemini-3.7-flash", "qwen/qwen3-plus"},
	}, "or")
	base := p
	p.FetchModelsHook = func(ctx context.Context, apiKey, baseURL string, timeout time.Duration) ([]string, bool) {
		return openrouterFetchModels(ctx, base, baseURL, timeout)
	}
	p.BuildExtraBodyHook = openrouterBuildExtraBody
	p.BuildAPIKwargsExtrasHook = openrouterBuildAPIKwargsExtras
	return []Profile{p}
}

// Public catalog fetched without auth and memoized for the process once a fetch succeeds
func openrouterFetchModels(ctx context.Context, base Profile, baseURL string, timeout time.Duration) ([]string, bool) {
	openrouterModelsMu.Lock()
	defer openrouterModelsMu.Unlock()
	if openrouterModelsSet {
		return openrouterModelsCache, true
	}
	ids, ok := base.FetchModels(ctx, "", baseURL, timeout)
	if ok {
		openrouterModelsCache, openrouterModelsSet = ids, true
	}
	return ids, ok
}

// Declared routing scope, then ambient conversation, then the explicit session id, as a cache scope
func openrouterStickyKey(ctx context.Context, sessionID string) string {
	return cacheScopeFromSessionID(firstNonEmpty(portalAffinityScope(ctx), portalConversationContext(ctx), sessionID))
}

// Sticky session key, provider preferences with the speed tier pin, and the pareto router plugin
func openrouterBuildExtraBody(sessionID string, kw map[string]any) map[string]any {
	body := map[string]any{}
	if sticky := openrouterStickyKey(hookCtx(kw), sessionID); sticky != "" {
		body["session_id"] = sticky
	}
	model := hookString(kw, "model")
	prefs, _ := kw["provider_preferences"].(map[string]any)
	if pin, ok := openRouterEndpointPins[model]; ok && !(pin.base == model && pyTruthy(prefs["only"])) {
		next := copyDict(prefs)
		next["only"] = append([]string(nil), pin.tags...)
		prefs = next
	}
	if len(prefs) > 0 {
		body["provider"] = prefs
	}
	score, present := kw["openrouter_min_coding_score"]
	if model == "openrouter/pareto-code" && present && score != nil && score != "" {
		if f, err := catalogFloat(score); err == nil && f >= 0 && f <= 1 {
			body["plugins"] = []any{map[string]any{"id": "pareto-router", "min_coding_score": f}}
		}
	}
	return body
}

// Reasoning passthrough clamped to the catalog, mandatory Anthropic effort as verbosity, Grok cache pin and tier slug rewrite
func openrouterBuildAPIKwargsExtras(reasoning *Reasoning, kw map[string]any) (map[string]any, map[string]any) {
	extraBody, topLevel := map[string]any{}, map[string]any{}
	ctx := hookCtx(kw)
	model := hookString(kw, "model")
	if pin, ok := openRouterEndpointPins[model]; ok && pin.base != model {
		topLevel["model"] = pin.base
	}
	if pyTruthy(kw["supports_reasoning"]) {
		switch {
		case anthropicReasoningIsMandatory(model):
			cfg := reasoningConfigMap(reasoning)
			effort, _ := cfg["effort"].(string)
			if enabled, set := cfg["enabled"]; (!set || enabled != false) && effort != "" && effort != "none" {
				topLevel["verbosity"] = effort
			}
		case reasoning != nil:
			if clamped := openrouterClampReasoningToCatalog(ctx, reasoningConfigMap(reasoning), model); clamped != nil {
				extraBody["reasoning"] = clamped
			}
		default:
			extraBody["reasoning"] = map[string]any{"enabled": true, "effort": "medium"}
		}
	}
	grokConvID := openrouterStickyKey(ctx, hookString(kw, "session_id"))
	if scope := hookString(kw, "cache_scope_id"); IsForkCacheScope(scope) {
		grokConvID = scope
	}
	if grokConvID != "" && model != "" && hasAnyPrefix(model, grokAggregatorModelPrefixes) {
		topLevel["extra_headers"] = map[string]string{"x-grok-conv-id": grokConvID}
	}
	return extraBody, topLevel
}

// Effort clamped to the catalog levels, nil when a disable meets a reasoning mandatory route
func openrouterClampReasoningToCatalog(ctx context.Context, cfg map[string]any, model string) map[string]any {
	effort, _ := cfg["effort"].(string)
	disabled := cfg["enabled"] == false || effort == "none"
	if effort == "" && !disabled {
		return cfg
	}
	caps, ok := OpenRouterModelReasoningCapabilities(ctx, model, 0, false)
	if !ok || !caps.SupportsReasoning {
		return cfg
	}
	if disabled {
		noDisable := strings.HasPrefix(model, "openai/") && !containsStr(codexSupportedEfforts(model), "none")
		if caps.Mandatory || noDisable {
			return nil
		}
		return cfg
	}
	clamped := ClampReasoningEffortToSupported(effort, caps.SupportedEfforts)
	if clamped != "" && clamped != effort {
		slog.Debug(fmt.Sprintf("openrouter: clamped reasoning effort %s \u2192 %s for %s (catalog supported_efforts=%s)",
			pyStringRepr(effort), pyStringRepr(clamped), model, openrouterEffortsRepr(caps.SupportedEfforts)))
		cfg = copyDict(cfg)
		cfg["effort"] = clamped
	}
	return cfg
}

func openrouterEffortsRepr(efforts []string) string {
	if efforts == nil {
		return "None"
	}
	return pyStrListRepr(efforts)
}
