package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Seconds before the in hand Nous key expires at which a fresh store key is adopted
const nousKeyAdoptSkewS = 180

// Turn boundary credential watch state of the env refresh and the Nous wire decision
type credentialRefreshState struct {
	envCredsSeen    *[2]string
	nousWireDecided bool
}

var decodeJWTClaims = provider.DecodeJWTClaims

var resolveNousCredentials = provider.ResolveNousRuntimeCredentialsWith

func clientLifecycleLog() *slog.Logger {
	return slog.Default().With("logger", "agent.client_lifecycle")
}

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

// Adopts a refreshed Nous key on either client kind, refusing a key minted for a different account
func (a *AIAgent) tryRefreshNousClientCredentials(ctx context.Context, force bool, requireAccount *string) (bool, error) {
	if a.provider != "nous" || (a.apiMode != "chat_completions" && a.apiMode != "anthropic_messages") {
		return false, nil
	}
	timeout := envFloatOr("ICEBATH_NOUS_TIMEOUT_SECONDS", 15)
	creds, err := resolveNousCredentials(agentPoolEnv(ctx), provider.NousResolveOptions{
		Timeout: time.Duration(timeout * float64(time.Second)), ForceRefresh: force, StaleAccessToken: a.apiKey,
	})
	if err != nil {
		clientLifecycleLog().Debug(fmt.Sprintf("Nous credential refresh failed: %v", err))
		return false, nil
	}
	if !validCredentialPair(creds["api_key"], creds["base_url"]) {
		return false, nil
	}
	apiKey, baseURL := pyStrValue(creds["api_key"]), pyStrValue(creds["base_url"])
	if strings.TrimSpace(apiKey) == strings.TrimSpace(a.apiKey) &&
		strings.TrimRight(strings.TrimSpace(baseURL), "/") == strings.TrimRight(strings.TrimSpace(a.baseURL), "/") {
		return false, nil
	}
	if requireAccount != nil {
		newAccount := ""
		if sub := decodeJWTClaims(apiKey)["sub"]; pyrepr.Truthy(sub) {
			newAccount = pyStrValue(sub)
		}
		if newAccount != *requireAccount {
			clientLifecycleLog().Info("Nous pre-expiry adoption skipped: the store's key belongs to a different account " +
				"than the one in hand; keeping the current credential.")
			return false, nil
		}
	}
	if a.apiMode == "anthropic_messages" {
		a.apiKey = strings.TrimSpace(apiKey)
		a.setBaseURL(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
		a.anthropicAPIKey, a.anthropicBaseURL = a.apiKey, a.baseURL
		if err := a.rebuildAnthropicClient(ctx); err != nil {
			return false, err
		}
		return true, nil
	}
	delete(a.clientKwargs, "default_headers")
	return a.adoptOpenAICredentials(apiKey, baseURL, "nous_credential_refresh"), nil
}

// Swaps in the store's fresh Nous key inside the expiry skew when it belongs to the same account
func (a *AIAgent) adoptNousKeyBeforeExpiry(ctx context.Context) bool {
	if a.provider != "nous" || a.apiKey == "" {
		return false
	}
	claims := decodeJWTClaims(a.apiKey)
	exp, account := claims["exp"], claims["sub"]
	expAt, numeric := jwtNumber(exp)
	if !pyrepr.Truthy(account) || !numeric || expAt-nowSeconds() > nousKeyAdoptSkewS {
		return false
	}
	want := pyStrValue(account)
	adopted, err := a.tryRefreshNousClientCredentials(ctx, false, &want)
	if err != nil {
		clientLifecycleLog().Debug("Nous key pre-expiry adoption failed", "error", err)
		return false
	}
	return adopted
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
	clientLifecycleLog().Info(fmt.Sprintf("Applied updated .env credentials for %s: endpoint %s", a.provider, a.baseURL))
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

// Route headers for the base url from host table, route hook or profile, then user and custom extras
func (a *AIAgent) applyClientHeadersForBaseURL(ctx context.Context, baseURL string, applyUserHeaders bool) error {
	if a.clientKwargs == nil {
		a.clientKwargs = map[string]any{}
	}
	if factory := hostDefaultHeadersFactory(baseURL); factory != nil {
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
			a.clientKwargs["default_headers"] = maps.Clone(routeHeaders)
		} else if profile, ok := provider.GetProviderProfile(ctx, a.provider); ok {
			if h := profile.DefaultHeaders(); len(h) > 0 {
				a.clientKwargs["default_headers"] = maps.Clone(h)
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
