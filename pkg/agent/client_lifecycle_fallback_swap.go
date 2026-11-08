package agent

import (
	"context"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Installs the fallback client in place honoring the route request timeout, the SDK default when unset
func swapFallbackClients(ctx context.Context, a *AIAgent, client provider.AuxClient, providerName, model, baseURL, apiMode string) error {
	timeout, hasTimeout := provider.GetProviderRequestTimeout(ctx, providerName, model)
	if providerName == "bedrock" && (apiMode == "anthropic_messages" || apiMode == "bedrock_converse") {
		return applyBedrockBinding(ctx, a, baseURL, provider.APIMode(apiMode))
	}
	credential := provider.AuxClientCredential(client)
	keySource, rotating := credential.(func() (string, error))
	staticKey, _ := credential.(string)
	if apiMode == "anthropic_messages" {
		var effective any = staticKey
		var tokenSource func(context.Context) (string, error)
		a.anthropicTokenSource = nil
		if rotating {
			a.anthropicTokenSource = provider.TagCredentialSource(func(context.Context) (string, error) { return keySource() }, "")
			effective, tokenSource = a.anthropicTokenSource, a.anthropicTokenSource.Mint
		} else if staticKey == "" && providerName == "anthropic" {
			effective = resolveAnthropicToken(agentPoolEnv(ctx), a.model)
		}
		effectiveKey, _ := effective.(string)
		a.apiKey, a.anthropicAPIKey = effectiveKey, effectiveKey
		a.anthropicBaseURL = baseURL
		built, err := anthropicClientBuilder(ctx, effective, baseURL, timeout, false)
		if err != nil {
			return err
		}
		a.anthropicClient = built
		a.isAnthropicOAuth = provider.AnthropicRouteIsOAuth(ctx, baseURL, effectiveKey, tokenSource, providerName)
		a.client, a.clientKwargs = nil, map[string]any{}
		return nil
	}
	a.apiKey = staticKey
	a.client = client
	headers := provider.AuxClientCustomHeaders(client)
	if len(headers) == 0 {
		headers = provider.AuxClientDefaultHeaders(client)
	}
	a.clientKwargs = map[string]any{"api_key": credential, "base_url": baseURL}
	if len(headers) > 0 {
		a.clientKwargs["default_headers"] = headers
	}
	if hasTimeout {
		a.clientKwargs["timeout"] = timeout
		a.replacePrimaryOpenAIClient("fallback_timeout_apply")
	}
	return nil
}
