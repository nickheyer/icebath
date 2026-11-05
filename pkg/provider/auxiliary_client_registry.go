package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	auxLoggedMinimaxAbsentKeys     = newAuxOnceSet()
	auxLoggedMinimaxUnexpectedKeys = newAuxOnceSet()
)

// Seams for the MiniMax OAuth credential resolver and the raw persisted provider state
var (
	auxMinimaxRuntimeCredentialsFn = resolveMinimaxOAuthRuntimeCredentialsWith
	auxProviderAuthStateFn         = func(env PoolEnv, provider string) (map[string]any, error) {
		return env.Store.GetProviderAuthState(provider)
	}
)

type auxRegistryArm func(ctx context.Context, req auxResolveRequest) (auxResolveResult, error)

// RegistryAuthTypeArms maps a registered auth type to the arm that builds and routes its client
var RegistryAuthTypeArms = map[string]auxRegistryArm{
	"vertex":            auxResolveVertexArm,
	"aws_sdk":           auxResolveBedrockArm,
	"oauth_minimax":     auxResolveMinimaxOAuthArm,
	"oauth_device_code": auxResolvePluginOAuthArm,
	"oauth_external":    auxResolvePluginOAuthArm,
}

// Vertex client for the request routed through the transport, empty when Vertex is not configured
func auxResolveVertexArm(ctx context.Context, req auxResolveRequest) (auxResolveResult, error) {
	client, finalModel := auxBuildVertexClient(ctx, req.Provider, req.Model)
	if client == nil {
		return auxResolveResult{}, nil
	}
	return auxRouteClient(ctx, req, client, finalModel), nil
}

// Bedrock client for the request routed through the transport, empty when AWS is not configured
func auxResolveBedrockArm(ctx context.Context, req auxResolveRequest) (auxResolveResult, error) {
	client, finalModel := auxBuildBedrockClient(ctx, req.Provider, req.Model, req.RawCodex)
	if client == nil {
		return auxResolveResult{}, nil
	}
	return auxRouteClient(ctx, req, client, finalModel), nil
}

// Logs a debug record the first time a key is seen and stays silent for every repeat
func auxLogOnceDebug(seen *auxOnceSet, key string, msg string, args ...any) {
	if !seen.add(key) {
		return
	}
	if len(args) > 0 {
		msg = fmt.Sprintf(msg, args...)
	}
	slog.Debug(msg)
}

// Anthropic wire client over a refreshing MiniMax OAuth bearer, empty when signed out or incomplete
func auxResolveMinimaxOAuthArm(ctx context.Context, req auxResolveRequest) (auxResolveResult, error) {
	env := modelsPoolEnv(ctx)
	finalModel := func() string {
		model := req.Model
		if model == "" {
			model = auxGetAuxModelForProvider(ctx, req.Provider)
		}
		if model == "" {
			model = "MiniMax-M3"
		}
		return auxNormalizeResolvedModel(model, req.Provider)
	}
	if auxProbeActive(ctx) {
		state, err := auxProviderAuthStateFn(env, "minimax-oauth")
		if err != nil {
			return auxResolveResult{}, err
		}
		baseURL := ""
		if v := state["inference_base_url"]; pyrepr.Truthy(v) {
			baseURL = strings.TrimRight(pystr.Strip(pyrepr.Str(v)), "/")
		}
		if !pyrepr.Truthy(state["access_token"]) || baseURL == "" {
			auxLogOnceDebug(auxLoggedMinimaxAbsentKeys, "minimax-oauth", "resolve_provider_client: minimax-oauth not logged in")
			return auxResolveResult{}, nil
		}
		return auxRouteClient(ctx, req, &auxProbeClient{apiKey: "", baseURL: baseURL}, finalModel()), nil
	}
	creds, err := auxMinimaxRuntimeCredentialsFn(env, true)
	if err != nil {
		var authErr *AuthError
		if errors.As(err, &authErr) {
			auxLogOnceDebug(auxLoggedMinimaxAbsentKeys, "minimax-oauth", "resolve_provider_client: minimax-oauth unavailable: %s", err)
			return auxResolveResult{}, nil
		}
		if auxLoggedMinimaxUnexpectedKeys.add("minimax-oauth") {
			slog.Warn(fmt.Sprintf("resolve_provider_client: unexpected minimax-oauth resolution failure: %s", err), "err", err)
		}
		return auxResolveResult{}, nil
	}
	tokenSource := auxCallableTokenSource(creds["api_key"])
	baseURL := ""
	if v := creds["base_url"]; pyrepr.Truthy(v) {
		baseURL = strings.TrimRight(pystr.Strip(pyrepr.Str(v)), "/")
	}
	if tokenSource == nil || baseURL == "" {
		auxLogOnceDebug(auxLoggedMinimaxAbsentKeys, "minimax-oauth", "resolve_provider_client: minimax-oauth credentials incomplete")
		return auxResolveResult{}, nil
	}
	model := finalModel()
	isOAuth := AnthropicRouteIsOAuth(ctx, baseURL, "", tokenSource, "")
	client, err := NewAnthropicAuxiliaryClient(ctx, model, creds["api_key"], baseURL, isOAuth)
	if err != nil {
		return auxResolveResult{}, err
	}
	return auxRouteClient(ctx, req, client, model), nil
}

// Context aware minting function of a callable credential, nil for a static or absent one
func auxCallableTokenSource(v any) func(context.Context) (string, error) {
	switch f := v.(type) {
	case *TaggedTokenSource:
		if f != nil {
			return f.Token
		}
	case func(context.Context) (string, error):
		return f
	case func() (string, error):
		return func(context.Context) (string, error) { return f() }
	}
	return nil
}

// Plugin OAuth client resolved through the signed in pool entry or configured endpoint
func auxResolvePluginOAuthArm(ctx context.Context, req auxResolveRequest) (auxResolveResult, error) {
	return auxResolvePluginOAuthClient(ctx, req), nil
}
