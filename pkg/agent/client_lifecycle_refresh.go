package agent

import (
	"context"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

// Recomputes TLS material and default headers for the current base url after it may have moved
func (a *AIAgent) reapplyRouteClientConfig(ctx context.Context, routeChanged bool) error {
	if a.clientKwargs == nil {
		a.clientKwargs = map[string]any{}
	}
	delete(a.clientKwargs, "ssl_verify")
	delete(a.clientKwargs, "ssl_ca_cert")
	provider.ApplyCustomProviderTLSToClientKwargs(a.clientKwargs, a.baseURL, provider.CustomProviderEntries(ctx))
	return a.applyClientHeadersForBaseURL(ctx, a.baseURL, !routeChanged)
}

// AI Gateway attribution headers for the routes that take them on a client rebuild
func aiGatewayRouteHeaders(context.Context, string, string) (map[string]string, error) {
	return provider.AIGatewayHeaders(), nil
}

// Route default headers a client rebuild applies, the first matching host winning
var routeDefaultHeadersTable = []hostDefaultHeaders{
	{"openrouter.ai", func(ctx context.Context, _, _ string) (map[string]string, error) {
		return provider.BuildORHeaders(ctx), nil
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
		if matched, _ := urlutil.BaseHostMatches(baseURL, entry.host); matched {
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
		headers, err := factory(ctx, anyutil.String(a.clientKwargs["api_key"]), baseURL)
		if err != nil {
			return err
		}
		a.clientKwargs["default_headers"] = headers
	} else {
		delete(a.clientKwargs, "default_headers")
		apiKey := ""
		if v := a.clientKwargs["api_key"]; anyutil.IsTrue(v) {
			apiKey = anyutil.String(v)
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
		provider.ApplyCustomProviderExtraHeadersToClientKwargs(a.clientKwargs, baseURL, provider.CustomProviderEntries(ctx))
	}
	return nil
}
