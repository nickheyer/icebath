package agent

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

var anthropicClientBuilder = provider.BuildAnthropicClient

var resolveAnthropicToken = provider.ResolveAnthropicToken

// Profile scoped credential surface the anthropic token resolver reads
func agentPoolEnv(ctx context.Context) provider.PoolEnv {
	h := home.Dir(ctx)
	return provider.PoolEnv{Store: provider.NewStore(h), Secrets: config.NewSecrets(h), Scope: provider.SecretScopeFrom(ctx), Config: provider.ConfigLoader(h), Log: slog.Default(), Ctx: ctx}
}

// Request timeout seconds for the agent route, zero when unset so the builder keeps its default
func (a *AIAgent) providerRequestTimeout(ctx context.Context) float64 {
	if t, ok := provider.GetProviderRequestTimeout(ctx, a.provider, a.model); ok {
		return t
	}
	return 0
}

// Native Anthropic client for the token and base URL with the provider and model request timeout
func (a *AIAgent) buildDirectAnthropicClient(ctx context.Context, token, baseURL string) (any, error) {
	return anthropicClientBuilder(ctx, token, baseURL, a.providerRequestTimeout(ctx), false)
}

// OAuth flag only on native Anthropic routes so third party Anthropic protocol endpoints never trip OAuth paths
func (a *AIAgent) anthropicOAuthFlag(ctx context.Context, token string) bool {
	return provider.AnthropicRouteIsOAuth(ctx, a.anthropicBaseURL, token, nil, a.provider)
}

// Closes a client that exposes Close, any failure or panic swallowed
func closeClientQuietly(client any) {
	c, ok := client.(interface{ Close() error })
	if !ok {
		return
	}
	defer func() { _ = recover() }()
	_ = c.Close()
}

// Engine surface carrying its own route key, the getattr api_key probe for engines beyond the built in compressor
type engineAPIKeyHolder interface {
	APIKey() string
	SetAPIKey(key string)
}

// Moves the context engine key off the revoked token when it still holds it
func rotateEngineAPIKey(ctx context.Context, cc ContextEngine, oldToken, newToken string) error {
	switch e := cc.(type) {
	case *ContextCompressor:
		if e.apiKey == oldToken {
			e.apiKey = newToken
		}
	case engineAPIKeyHolder:
		if e.APIKey() == oldToken {
			e.SetAPIKey(newToken)
		}
	case *hostedContextEngine:
		v, err := e.obj.Get(ctx, "api_key")
		var builtin *plugins.HostBuiltinError
		if errors.As(err, &builtin) && builtin.Type == "AttributeError" {
			return nil
		}
		if err != nil {
			return err
		}
		if v != any(oldToken) {
			return nil
		}
		return e.obj.Set(ctx, "api_key", newToken)
	}
	return nil
}

// Rotates a native Anthropic OAuth or sk-ant token in place, rebuilding the client and moving every holder of the old token
func (a *AIAgent) tryRefreshAnthropicClientCredentials(ctx context.Context) (bool, error) {
	baseURL := a.anthropicBaseURL
	if a.apiMode != "anthropic_messages" || (a.anthropicClient == nil && a.anthropicAPIKey == "") || a.provider != "anthropic" {
		return false, nil
	}
	azure, err := pyurl.BaseURLHostMatches(baseURL, "azure.com")
	if err != nil {
		return false, err
	}
	if azure {
		return false, nil
	}
	official := baseURL == ""
	for _, host := range []string{"anthropic.com", "claude.com"} {
		if official {
			break
		}
		if official, err = pyurl.BaseURLHostMatches(baseURL, host); err != nil {
			return false, err
		}
	}
	if !official && !strings.HasPrefix(a.anthropicAPIKey, "sk-ant-") && !a.isAnthropicOAuth {
		return false, nil
	}
	newToken := strings.TrimSpace(resolveAnthropicToken(agentPoolEnv(ctx), a.model))
	if newToken == "" || newToken == a.anthropicAPIKey {
		return false, nil
	}
	closeClientQuietly(a.anthropicClient)
	client, err := a.buildDirectAnthropicClient(ctx, newToken, baseURL)
	if err != nil {
		slog.Warn("Failed to rebuild Anthropic client after credential refresh: " + err.Error())
		return false, nil
	}
	a.anthropicClient = client
	oldToken := a.anthropicAPIKey
	a.anthropicAPIKey, a.isAnthropicOAuth = newToken, a.anthropicOAuthFlag(ctx, newToken)
	if a.apiKey == oldToken {
		a.apiKey = newToken
	}
	if a.contextCompressor != nil {
		if err := rotateEngineAPIKey(ctx, a.contextCompressor, oldToken, newToken); err != nil {
			return false, err
		}
	}
	if rt := a.primaryRuntime; rt != nil {
		if rt["anthropic_api_key"] == any(oldToken) {
			rt["is_anthropic_oauth"] = a.isAnthropicOAuth
		}
		for _, k := range []string{"api_key", "anthropic_api_key", "compressor_api_key"} {
			if rt[k] == any(oldToken) {
				rt[k] = newToken
			}
		}
	}
	provider.RotateRuntimeMainAPIKey(ctx, oldToken, newToken)
	return true, nil
}
