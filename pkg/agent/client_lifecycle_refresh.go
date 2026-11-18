package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Turn boundary credential watch state of the env refresh
type credentialRefreshState struct {
	envCredsSeen *[2]string
}

var decodeJWTClaims = provider.DecodeJWTClaims

// Python str of an arbitrary value, empty for nil
func pyStrValue(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}

// Numeric JWT claim the way isinstance int or float accepts it, booleans included
func jwtNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// Adopts only env edits since the last look, never drift from pool rotation or a configured base url
func (a *AIAgent) shouldAdoptEnvCredentials(apiKey, baseURL, defaultBase string) bool {
	prev := a.envCredsSeen
	currentBase := strings.TrimRight(strings.TrimSpace(a.baseURL), "/")
	unchanged := baseURL == currentBase && apiKey == a.apiKey
	if prev == nil {
		poolRotated := apiKey != a.apiKey && a.credentialPool != nil && a.credentialPoolEntryID != ""
		return currentBase == defaultBase && !unchanged && !poolRotated
	}
	return (baseURL != prev[0] || apiKey != prev[1]) && (currentBase == defaultBase || currentBase == prev[0]) && !unchanged
}

// Adopts .env credential and base url edits at the turn boundary, rolling back when the client rebuild fails
func (a *AIAgent) tryRefreshEnvClientCredentials(ctx context.Context) (bool, error) {
	if a.apiMode != "chat_completions" || a.fallbackActivated {
		return false, nil
	}
	apiKey, baseURL, defaultBase, ok := provider.ResolveEnvCredentials(agentPoolEnv(ctx), a.provider, a.requestedProvider)
	if !ok {
		return false, nil
	}
	if !a.shouldAdoptEnvCredentials(apiKey, baseURL, defaultBase) {
		a.envCredsSeen = &[2]string{baseURL, apiKey}
		return false, nil
	}
	routeChanged := provider.NormalizeRouteBaseURL(a.baseURL) != provider.NormalizeRouteBaseURL(baseURL)
	priorAPIKey, priorBaseURL := a.apiKey, a.baseURL
	priorClientKwargs := maps.Clone(a.clientKwargs)
	a.apiKey = apiKey
	a.setBaseURL(baseURL)
	a.syncClientKwargsCredentials()
	if err := a.reapplyRouteClientConfig(ctx, routeChanged); err != nil {
		return false, err
	}
	if !a.replacePrimaryOpenAIClient("env_credential_refresh") {
		a.apiKey = priorAPIKey
		a.setBaseURL(priorBaseURL)
		clear(a.clientKwargs)
		maps.Copy(a.clientKwargs, priorClientKwargs)
		return false, nil
	}
	syncCredentialPoolEntryID(a)
	a.envCredsSeen = &[2]string{baseURL, apiKey}
	pkgLog().Info(fmt.Sprintf("Applied updated .env credentials for %s: endpoint %s", a.provider, a.baseURL))
	return true, nil
}

// Recomputes TLS material and default headers for the current base url after it may have moved
func (a *AIAgent) reapplyRouteClientConfig(ctx context.Context, routeChanged bool) error {
	if a.clientKwargs == nil {
		a.clientKwargs = map[string]any{}
	}
	delete(a.clientKwargs, "ssl_verify")
	delete(a.clientKwargs, "ssl_ca_cert")
	cfg := provider.LoadConfigSafe(ctx, home.Dir(ctx))
	provider.ApplyCustomProviderTLSToClientKwargs(a.clientKwargs, a.baseURL, provider.CompatibleCustomProviders(cfg), cfg)
	return a.applyClientHeadersForBaseURL(ctx, a.baseURL, !routeChanged)
}

// AI Gateway attribution headers for the routes that take them on a client rebuild
func aiGatewayRouteHeaders(context.Context, string, string) (map[string]string, error) {
	return provider.AIGatewayHeaders(), nil
}

// Route default headers a client rebuild applies, the first matching host winning
var routeDefaultHeadersTable = []hostDefaultHeaders{
	{"openrouter.ai", func(ctx context.Context, _, _ string) (map[string]string, error) {
		return provider.BuildORHeaders(ctx, nil)
	}},
	{"ai-gateway.vercel.sh", aiGatewayRouteHeaders},
	{"integrate.api.nvidia.com", func(_ context.Context, _, base string) (map[string]string, error) {
		return provider.BuildNvidiaNIMHeaders(base), nil
	}},
	{"api.routermint.com", func(context.Context, string, string) (map[string]string, error) { return routermintHeaders() }},
	{"api.kimi.com", aiGatewayRouteHeaders},
	{"portal.qwen.ai", func(context.Context, string, string) (map[string]string, error) { return qwenPortalHeaders(), nil }},
	{"chatgpt.com", func(_ context.Context, key, base string) (map[string]string, error) {
		return provider.CodexCloudflareHeaders(key, base), nil
	}},
	{"x.ai", func(context.Context, string, string) (map[string]string, error) {
		return tools.XAIDefaultHeaders(), nil
	}},
}

// Header factory of the first rebuild table host the base url matches, nil when none does
func routeDefaultHeadersFactory(baseURL string) hostHeadersFactory {
	for _, entry := range routeDefaultHeadersTable {
		if matched, _ := pyurl.BaseURLHostMatches(baseURL, entry.host); matched {
			return entry.factory
		}
	}
	return nil
}

// Route headers for the base url from host table, route hook or profile, then user and custom extras
func (a *AIAgent) applyClientHeadersForBaseURL(ctx context.Context, baseURL string, applyUserHeaders bool) error {
	if a.clientKwargs == nil {
		a.clientKwargs = map[string]any{}
	}
	if factory := routeDefaultHeadersFactory(baseURL); factory != nil {
		headers, err := factory(ctx, pyStrValue(a.clientKwargs["api_key"]), baseURL)
		if err != nil {
			return err
		}
		a.clientKwargs["default_headers"] = headers
	} else {
		delete(a.clientKwargs, "default_headers")
		apiKey := ""
		if v := a.clientKwargs["api_key"]; pyrepr.Truthy(v) {
			apiKey = pyStrValue(v)
		}
		routeHeaders := stringHeaders(provider.RouteAnswer(ctx, "client_headers", a.provider, a, map[string]any{"base_url": baseURL, "api_key": apiKey}))
		if len(routeHeaders) > 0 {
			a.clientKwargs["default_headers"] = routeHeaders
		} else if profile, ok := provider.GetProviderProfile(ctx, a.provider); ok {
			if h := profile.DefaultHeaders(); len(h) > 0 {
				a.clientKwargs["default_headers"] = stringHeaders(h)
			}
		}
	}
	if applyUserHeaders {
		a.applyUserDefaultHeaders(ctx)
	}
	if a.apiMode != "anthropic_messages" && a.apiMode != "bedrock_converse" {
		cfg := provider.LoadConfigSafe(ctx, home.Dir(ctx))
		provider.ApplyCustomProviderExtraHeadersToClientKwargs(a.clientKwargs, baseURL, nil, cfg)
	}
	return nil
}
