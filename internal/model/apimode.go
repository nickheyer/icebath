package model

import (
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/auth/azure"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const defaultEntraScope = azure.DefaultScope

var (
	hostMandatedModes = map[string]wire.Mode{
		"api.x.ai": wire.ModeResponses, "api.meta.ai": wire.ModeResponses, "api.actual.inc": wire.ModeChat,
		"api.router.com": wire.ModeResponses, "api.anthropic.com": wire.ModeAnthropic,
	}
	validModes = []wire.Mode{wire.ModeChat, wire.ModeResponses, wire.ModeAnthropic, wire.ModeBedrock, wire.ModeCodexAppServer}
)

func isAlnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func isAlpha(r rune) bool { return unicode.IsLetter(r) }

func firstMode(modes ...wire.Mode) wire.Mode {
	for _, m := range modes {
		if m != "" {
			return m
		}
	}
	return ""
}

func detectModeForURL(baseURL string) wire.Mode {
	normalized := strings.TrimRight(strings.ToLower(strings.TrimSpace(baseURL)), "/")
	host := providers.URLHostname(baseURL)
	if m, ok := hostMandatedModes[host]; ok {
		return m
	}
	if providers.IsOfficialOpenAIHost(baseURL) {
		return wire.ModeResponses
	}
	path := providers.URLPath(normalized)
	if strings.HasSuffix(path, "/anthropic") || strings.HasSuffix(path, "/anthropic/v1") ||
		(host == "api.kimi.com" && strings.Contains(normalized, "/coding")) {
		return wire.ModeAnthropic
	}
	return ""
}

func (x *resolution) parseMode(raw string) wire.Mode {
	normalized := wire.Mode(strings.ToLower(config.CanonicalAPIMode(raw)))
	if normalized == "" {
		return ""
	}
	if slices.Contains(validModes, normalized) || slices.Contains(x.o().Dialects(x.ctx), normalized) {
		return normalized
	}
	return ""
}

func (x *resolution) fallbackMode(provider, baseURL, model string) wire.Mode {
	if x.o().Providers.IsActualRoute(x.ctx, provider, baseURL) {
		return wire.ModeChat
	}
	if detected := detectModeForURL(baseURL); detected != "" {
		return detected
	}
	declared := x.o().Catalog.DetermineMode(x.ctx, provider, baseURL, model)
	if declared == wire.ModeAnthropic && !x.onDeclaredAnthropicEndpoint(provider, baseURL) {
		return wire.ModeChat
	}
	return declared
}

func (x *resolution) onDeclaredAnthropicEndpoint(provider, baseURL string) bool {
	def, _ := x.o().Catalog.Provider(x.ctx, provider, false)
	defaultURL := strings.TrimSpace(def.BaseURL)
	defaultPath := strings.TrimSuffix(providers.URLPath(defaultURL), "/v1")
	if defaultPath == "" || strings.TrimSpace(baseURL) == "" {
		return true
	}
	if !providers.HostMatches(baseURL, providers.URLHostname(defaultURL)) {
		return false
	}
	path := providers.URLPath(baseURL)
	return path == "" || path == defaultPath || strings.HasPrefix(path, defaultPath+"/")
}

func (x *resolution) plainCustomMode(mc *modelCfg, baseURL string) wire.Mode {
	if x.o().Providers.IsActualRoute(x.ctx, "", baseURL) {
		return wire.ModeChat
	}
	configured := x.parseMode(mc.APIMode)
	detected := detectModeForURL(baseURL)
	if configured == wire.ModeResponses && detected != wire.ModeResponses {
		shown := baseURL
		if shown == "" {
			shown = "(unknown)"
		}
		slog.Info("Ignoring persisted custom api_mode=codex_responses for non-OpenAI endpoint " + shown)
		configured = ""
	}
	return firstMode(configured, detected, wire.ModeChat)
}

func (x *resolution) sameRegistered(provider, configured string) bool {
	if provider == configured {
		return true
	}
	reg := x.o().Providers
	p, ok := reg.RegistryRow(x.ctx, provider)
	if !ok {
		return false
	}
	c, ok := reg.RegistryRow(x.ctx, configured)
	return ok && p.ID == c.ID
}

func (x *resolution) supportsExplicitMode(provider, configured string) bool {
	p, c := strings.ToLower(strings.TrimSpace(provider)), strings.ToLower(strings.TrimSpace(configured))
	if c == "" {
		return true
	}
	if p == "custom" {
		return c == "custom" || strings.HasPrefix(c, "custom:")
	}
	return x.sameRegistered(p, c)
}

func (x *resolution) configuredMode(provider string, mc *modelCfg) wire.Mode {
	if m := x.parseMode(mc.APIMode); m != "" && x.supportsExplicitMode(provider, cfgProvider(mc)) {
		return m
	}
	return ""
}

func (x *resolution) perModelProfile(provider string) providers.Profile {
	if provider == "" {
		return nil
	}
	p, ok := x.o().Providers.Get(x.ctx, provider)
	if !ok || !providers.Overrides(p, providers.HookModeForModel) {
		return nil
	}
	return p
}

func (x *resolution) profileRuntimeMode(provider string, mc *modelCfg, apiKey, target string) wire.Mode {
	reg := x.o().Providers
	profile := x.perModelProfile(provider)
	configured := x.configuredMode(provider, mc)
	model := strings.TrimSpace(effectiveModel(mc, target))
	if configured != "" {
		if configured == wire.ModeResponses && model != "" && profile != nil {
			if m, ok := reg.ModeForModel(x.ctx, profile, model, nil); ok && m == wire.ModeChat {
				return wire.ModeChat
			}
		}
		return configured
	}
	if profile == nil {
		return wire.ModeChat
	}
	declared := profile.Spec().ModeOrDefault()
	if model == "" {
		return declared
	}
	key := auth.NewSecret(apiKey)
	if m, ok := reg.ModeForModel(x.ctx, profile, model, &key); ok && m != "" {
		return m
	}
	return declared
}

func azureInferredMode(model string, mode wire.Mode) wire.Mode {
	if model == "" || mode == wire.ModeAnthropic {
		return mode
	}
	if m, ok := providers.AzureFoundryModelMode(model); ok {
		return m
	}
	return mode
}

func (x *resolution) configuredOrFallbackMode(provider string, mc *modelCfg, baseURL, model string, opencodeByModel bool) wire.Mode {
	if provider == "actual" {
		if configured := x.configuredMode(provider, mc); configured != "" && configured != wire.ModeChat {
			slog.Info("Routing built-in Actual through chat_completions instead of persisted api_mode=" + string(configured))
		}
		return wire.ModeChat
	}
	reg := x.o().Providers
	if opencodeByModel {
		if _, ok := reg.OpenCodeFamily(x.ctx, provider); ok {
			return reg.OpenCodeModelMode(x.ctx, provider, model)
		}
	}
	if m := x.configuredMode(provider, mc); m != "" {
		return m
	}
	return x.fallbackMode(provider, baseURL, model)
}

func (x *resolution) apiKeyProviderMode(provider string, mc *modelCfg, apiKey, baseURL, model string, opencodeByModel bool) wire.Mode {
	if x.perModelProfile(provider) != nil {
		return x.profileRuntimeMode(provider, mc, apiKey, model)
	}
	if provider == "xai" {
		return wire.ModeResponses
	}
	return x.configuredOrFallbackMode(provider, mc, baseURL, model, opencodeByModel)
}

func (x *resolution) codexAppServer(provider string, mode wire.Mode, mc *modelCfg, requested string) wire.Mode {
	if strings.ToLower(strings.TrimSpace(mc.OpenAIRuntime)) != string(wire.ModeCodexAppServer) {
		return mode
	}
	if slices.Contains(openAIRuntimeProviders, provider) || slices.Contains(openAIRuntimeProviders, requested) {
		return wire.ModeCodexAppServer
	}
	if provider == "custom" {
		if id, ok := x.codexProviderID(requested); ok && id != "" {
			return wire.ModeCodexAppServer
		}
	}
	return mode
}

func agentKeyUsable(token, scope, expiresAt string, minTTL time.Duration) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	raw, ok := auth.JWTClaims(token)
	if !ok {
		return false
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil || len(claims) == 0 {
		return false
	}
	scopes := scopeSet(scope)
	for _, k := range []string{"scope", "scp"} {
		for s := range scopeSet(claims[k]) {
			scopes[s] = true
		}
	}
	if !scopes[auth.PortalInferenceInvokeScope] {
		return false
	}
	skew := max(0, minTTL.Truncate(time.Second))
	if exp, ok := claimNumber(claims["exp"]); ok {
		return exp > float64(time.Now().UnixNano())/1e9+skew.Seconds()
	}
	return !auth.IsExpiring(expiresAt, skew)
}

func scopeSet(raw any) map[string]bool {
	out := map[string]bool{}
	switch t := raw.(type) {
	case string:
		for _, part := range strings.Fields(strings.ReplaceAll(t, ",", " ")) {
			out[part] = true
		}
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok {
				for k := range scopeSet(s) {
					out[k] = true
				}
			}
		}
	}
	return out
}

func claimNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case float64:
		return t, true
	}
	return 0, false
}
