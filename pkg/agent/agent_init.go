package agent

import (
	"context"
	"errors"
	"maps"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Known exact default routes for a canonical provider id
func providerDefaultRoutes(ctx context.Context, providerName string) map[string]bool {
	routes := map[string]bool{}
	add := func(value string) {
		if route := provider.NormalizeRouteBaseURL(value); route != "" {
			routes[route] = true
		}
	}
	if overlay, ok := provider.IcebathOverlays(ctx)[providerName]; ok {
		add(overlay.BaseURLOverride)
	}
	if def := provider.GetProviderDef(ctx, providerName, false); def != nil {
		add(def.BaseURL)
	}
	if profile, ok := provider.GetProviderProfile(ctx, providerName); ok {
		add(profile.BaseURL)
	}
	for _, row := range provider.RegistryInferenceRoutes(ctx) {
		if provider.NormalizeProviderDef(ctx, provider.NormalizeProvider(row[0])) == providerName {
			add(row[1])
		}
	}
	if providerName == "gemini" {
		for _, route := range slicesOfKeys(routes) {
			routes[strings.TrimRight(route, "/")+"/openai"] = true
		}
	}
	return routes
}

func slicesOfKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Whether a context pin configured route differs from the runtime route
func contextRouteMismatch(ctx context.Context, configuredBaseURL, activeBaseURL, configuredProvider, activeProvider string, alreadyNormalized bool) bool {
	norm := provider.NormalizeRouteBaseURL
	if alreadyNormalized {
		norm = func(v string) string { return v }
	}
	configuredRoute, activeRoute := norm(configuredBaseURL), norm(activeBaseURL)
	if configuredRoute != "" {
		return configuredRoute != activeRoute
	}
	configuredProvider, activeProvider = strings.TrimSpace(configuredProvider), strings.TrimSpace(activeProvider)
	if configuredProvider == "" {
		return false
	}
	configuredProvider = provider.NormalizeProviderDef(ctx, provider.NormalizeProvider(configuredProvider))
	activeProvider = provider.NormalizeProviderDef(ctx, provider.NormalizeProvider(activeProvider))
	if activeRoute != "" {
		if configuredRoutes := providerDefaultRoutes(ctx, configuredProvider); len(configuredRoutes) > 0 {
			return !configuredRoutes[activeRoute]
		}
		return !(activeProvider != "" && configuredProvider == activeProvider)
	}
	return configuredProvider != "" && activeProvider != "" && configuredProvider != activeProvider
}

// Runtime normalization of a requested custom provider identity
func normalizeCustomProviderName(value any) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(initStr(value))), " ", "-")
}

// Raw and menu identities runtime accepts for a configured custom provider name
func customProviderRuntimeIDs(value any) map[string]bool {
	normalized := normalizeCustomProviderName(value)
	if normalized == "" {
		return map[string]bool{}
	}
	return map[string]bool{normalized: true, "custom:" + normalized: true}
}

// Python str of value or of the empty string when falsy
func initStr(value any) string {
	if !pyrepr.Truthy(value) {
		return ""
	}
	return pyrepr.Str(value)
}

// Global threshold merged with a per model override, a codex autoraise notice only when it raises
func resolveCompressionThreshold(globalThreshold float64, modelCthresh *float64, model string, isCodexAutoraise bool) (float64, map[string]any) {
	if modelCthresh == nil {
		return globalThreshold, nil
	}
	if isCodexAutoraise {
		if *modelCthresh <= globalThreshold+1e-9 {
			return globalThreshold, nil
		}
		return *modelCthresh, map[string]any{"model": model, "from": globalThreshold, "to": *modelCthresh}
	}
	return *modelCthresh, nil
}

func normalizedCustomBaseURL(value any) string {
	s, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(s), "/")
}

// Whether a custom provider entry serves the agent model by catalog or by its model field
func customProviderModelMatches(agentModel string, entry map[string]any) bool {
	norm := strings.ToLower(strings.TrimSpace(agentModel))
	var catalog []string
	switch m := entry["models"].(type) {
	case map[string]any:
		for k := range m {
			catalog = append(catalog, strings.ToLower(strings.TrimSpace(k)))
		}
	case []any:
		for _, v := range m {
			catalog = append(catalog, strings.ToLower(strings.TrimSpace(pyrepr.Str(v))))
		}
	}
	for _, c := range catalog {
		if c == norm {
			return true
		}
	}
	providerModel := strings.ToLower(strings.TrimSpace(initStr(entry["model"])))
	return (providerModel == "" && len(catalog) == 0) || providerModel == norm
}

// Extra body of the custom provider entry matching the agent route, model specific entries first
func customProviderExtraBodyForAgent(providerName, model, baseURL string, customProviders []map[string]any) map[string]any {
	providerNorm := strings.ToLower(strings.TrimSpace(providerName))
	if providerNorm != "custom" && !strings.HasPrefix(providerNorm, "custom:") {
		return nil
	}
	keyFilter := ""
	if _, after, ok := strings.Cut(providerNorm, ":"); ok {
		keyFilter = strings.TrimSpace(after)
	}
	targetURL := normalizedCustomBaseURL(baseURL)
	if targetURL == "" {
		return nil
	}
	var fallback map[string]any
	for _, entry := range customProviders {
		if entry == nil {
			continue
		}
		entryKeys := map[string]bool{
			strings.ToLower(strings.TrimSpace(initStr(entry["provider_key"]))): true,
			strings.ToLower(strings.TrimSpace(initStr(entry["name"]))):         true,
		}
		if keyFilter != "" && !entryKeys[keyFilter] {
			continue
		}
		if normalizedCustomBaseURL(entry["base_url"]) != targetURL {
			continue
		}
		extraBody, ok := entry["extra_body"].(map[string]any)
		if !ok || len(extraBody) == 0 {
			continue
		}
		if strings.TrimSpace(initStr(entry["model"])) != "" {
			if customProviderModelMatches(model, entry) {
				return maps.Clone(extraBody)
			}
		} else if fallback == nil {
			fallback = maps.Clone(extraBody)
		}
	}
	return fallback
}

// Merges the matching custom provider extra_body under the agent request overrides
func (a *AIAgent) mergeCustomProviderExtraBody(customProviders []map[string]any) {
	extraBody := customProviderExtraBodyForAgent(a.provider, a.model, a.baseURL, customProviders)
	if len(extraBody) == 0 {
		return
	}
	overrides := maps.Clone(a.requestOverrides)
	if overrides == nil {
		overrides = map[string]any{}
	}
	merged := maps.Clone(extraBody)
	if existing, ok := overrides["extra_body"].(map[string]any); ok {
		maps.Copy(merged, existing)
	}
	overrides["extra_body"] = merged
	a.requestOverrides = overrides
}

// Positive seconds of a run budget, nil turns the feature off and a bool is rejected
func normalizeRunBudgetSeconds(value any) *float64 {
	var seconds float64
	switch v := value.(type) {
	case nil, bool:
		return nil
	case int:
		seconds = float64(v)
	case int64:
		seconds = float64(v)
	case float64:
		seconds = v
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return nil
		}
		seconds = f
	default:
		return nil
	}
	if math.IsNaN(seconds) || seconds <= 0 {
		return nil
	}
	return &seconds
}

// Fails closed when a required checkpoint meets the codex app server that compacts its own thread
func refuseCheckpointRequiredOnCodexAppServer(checkpointRequired bool, apiMode string) error {
	if checkpointRequired && apiMode == "codex_app_server" {
		return errors.New("BLOCKED_MISSING_PREREQUISITE: compression.checkpoint_required " +
			"is incompatible with the codex_app_server API mode: the codex " +
			"agent compacts its own thread without a truthful pre-compaction " +
			"transcript boundary, so a required pre-compress checkpoint " +
			"cannot be guaranteed. Disable compression.checkpoint_required " +
			"or use a non-app-server API mode.")
	}
	return nil
}

// Strict int coercion rejecting bool and fractional floats
func parseConfigInt(raw any, def int) int {
	switch v := raw.(type) {
	case bool:
		return def
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		if v == math.Trunc(v) && !math.IsInf(v, 0) {
			return int(v)
		}
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(initStr(raw)))
	if err != nil {
		return def
	}
	return n
}

// Legacy string set truthiness of a compression section key
func cfgFlag(cfg map[string]any, key string, def bool) bool {
	v, ok := cfg[key]
	if !ok {
		v = def
	}
	switch strings.ToLower(pyBoolStr(v)) {
	case "true", "1", "yes":
		return true
	}
	return false
}

// Section mapping or empty when malformed
func cfgDict(cfg map[string]any, key string) map[string]any {
	if section, ok := cfg[key].(map[string]any); ok {
		return section
	}
	return map[string]any{}
}

var explicitAPIModes = map[string]bool{"chat_completions": true, "codex_responses": true, "anthropic_messages": true, "bedrock_converse": true, "codex_app_server": true}

// Sets the api mode and provider rewrites from an ordered ladder where the first match wins
func (a *AIAgent) resolveAPIMode(ctx context.Context, apiMode string, providerName *string, baseURL string) {
	host, url := a.baseURLHostname, a.baseURLLower
	amazon, _ := pyurl.BaseURLHostMatches(url, "amazonaws.com")
	switch {
	case provider.IsActualRoute(a.provider, baseURL):
		a.apiMode = "chat_completions"
	case explicitAPIModes[apiMode] || (apiMode != "" && provider.RegisteredAPIModes()[apiMode]):
		a.apiMode = apiMode
	case a.provider == "openai-codex" || a.provider == "xai" || a.provider == "xai-oauth":
		a.apiMode = "codex_responses"
	case providerName == nil && host == "chatgpt.com" && strings.Contains(url, "/backend-api/codex"):
		a.apiMode, a.provider = "codex_responses", "openai-codex"
	case providerName == nil && host == "api.x.ai":
		a.apiMode, a.provider = "codex_responses", "xai"
	case a.provider == "anthropic" || (providerName == nil && host == "api.anthropic.com"):
		a.apiMode, a.provider = "anthropic_messages", "anthropic"
	case strings.HasSuffix(strings.TrimRight(url, "/"), "/anthropic"):
		a.apiMode = "anthropic_messages"
	case a.provider == "bedrock" || (strings.HasPrefix(host, "bedrock-runtime.") && amazon):
		a.apiMode = "bedrock_converse"
	case a.provider == "nous" || a.provider == "nous-portal" || a.provider == "nousresearch":
		a.apiMode = string(provider.NousMode(a.model, nousAnthropicWire(ctx)))
	default:
		a.apiMode = "chat_completions"
		if mandated := provider.HostMandatedMode(baseURL); mandated != "" {
			a.apiMode = string(mandated)
		}
	}
}

// Configured nous anthropic wire, chat when unreadable or unset
func nousAnthropicWire(ctx context.Context) string {
	cfg, err := config.LoadReadonly(ctx, home.Dir(ctx), os.LookupEnv)
	if err != nil {
		return "chat"
	}
	value := strings.ToLower(strings.TrimSpace(initStr(cfg.Get("nous.anthropic_wire"))))
	if value == "native" || value == "auto" {
		return value
	}
	return "chat"
}
