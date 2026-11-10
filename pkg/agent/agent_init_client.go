package agent

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Native Anthropic or AnthropicBedrock client with the per request MiniMax token source when that route asks for one
func initAnthropicClient(ctx context.Context, a *AIAgent, apiKey, baseURL string, timeout *float64) error {
	a.client = nil
	a.clientKwargs = map[string]any{}
	a.anthropicBaseURL = baseURL
	if a.provider == "bedrock" {
		if err := applyBedrockBinding(ctx, a, baseURL, provider.ModeAnthropicMessages); err != nil {
			return err
		}
		if !a.quietMode {
			_, _ = fmt.Fprintf(statusStdout, "🤖 AI Agent initialized with model: %s (AWS Bedrock + AnthropicBedrock SDK, %s)\n", a.model, a.bedrockRegion)
		}
		return nil
	}
	effective := apiKey
	if effective == "" && a.provider == "anthropic" {
		effective = provider.ResolveAnthropicToken(agentPoolEnv(ctx), a.model)
	}
	var key any = effective
	var tokenSource func(context.Context) (string, error)
	a.anthropicTokenSource = nil
	if a.provider == "minimax-oauth" && effective != "" {
		src := provider.BuildMinimaxOAuthTokenProvider(agentPoolEnv(ctx))
		a.anthropicTokenSource, key, tokenSource = src, src, src.Mint
	}
	a.apiKey, a.anthropicAPIKey = effective, effective
	a.isAnthropicOAuth = provider.AnthropicRouteIsOAuth(ctx, baseURL, effective, tokenSource, a.provider)
	t := 0.0
	if timeout != nil {
		t = *timeout
	}
	client, err := anthropicClientBuilder(ctx, key, baseURL, t, false)
	if err != nil {
		return err
	}
	a.anthropicClient = client
	if !a.quietMode {
		_, _ = fmt.Fprintf(statusStdout, "🤖 AI Agent initialized with model: %s (Anthropic native)\n", a.model)
		printKeyBanner(key, "token", false)
	}
	return nil
}

// Region, guardrail config, credential and client of the Bedrock runtime bound to the agent
func applyBedrockBinding(ctx context.Context, a *AIAgent, baseURL string, mode provider.APIMode) error {
	b, err := provider.BindBedrockRuntime(ctx, baseURL, mode, a.model)
	if err != nil {
		return err
	}
	a.bedrockRegion, a.bedrockGuardrailConfig = b.Region, b.GuardrailConfig
	a.client, a.clientKwargs = nil, map[string]any{}
	a.apiKey, a.anthropicAPIKey = b.APIKey, b.APIKey
	a.setBaseURL(b.BaseURL)
	a.anthropicBaseURL = b.BaseURL
	a.isAnthropicOAuth = b.IsOAuth
	a.anthropicClient = nil
	if b.Client != nil {
		a.anthropicClient = b.Client
	}
	return nil
}

// Mixture of agents facade in place of an HTTP client
func initMoaClient(ctx context.Context, a *AIAgent, apiKey string) error {
	var key any
	if apiKey != "" {
		key = apiKey
	}
	if err := bindMoaRuntime(ctx, a, a.model, key); err != nil {
		return err
	}
	if !a.quietMode {
		_, _ = fmt.Fprintf(statusStdout, "🤖 AI Agent initialized with MoA preset: %s\n", a.model)
	}
	return nil
}

// Bedrock Converse binding without an OpenAI client
func initBedrockClient(ctx context.Context, a *AIAgent, baseURL string) error {
	if err := applyBedrockBinding(ctx, a, baseURL, provider.APIMode("bedrock_converse")); err != nil {
		return err
	}
	if !a.quietMode {
		label := ""
		if len(a.bedrockGuardrailConfig) > 0 {
			label = " + Guardrails"
		}
		_, _ = fmt.Fprintf(statusStdout, "🤖 AI Agent initialized with model: %s (AWS Bedrock, %s%s)\n", a.model, a.bedrockRegion, label)
	}
	return nil
}

// Client kwargs from explicit credentials with the query split out, launch args and the route headers
func explicitClientKwargs(ctx context.Context, a *AIAgent, apiKey, baseURL string, timeout *float64) (map[string]any, error) {
	kwargs := map[string]any{"api_key": apiKey, "base_url": baseURL}
	if u, err := url.Parse(baseURL); err == nil && u.RawQuery != "" {
		query := map[string]string{}
		if values, qerr := url.ParseQuery(u.RawQuery); qerr == nil {
			for k, v := range values {
				for _, item := range v {
					if item != "" {
						query[k] = item
						break
					}
				}
			}
		}
		u.RawQuery, u.ForceQuery = "", false
		kwargs["base_url"], kwargs["default_query"] = u.String(), query
	}
	if timeout != nil {
		kwargs["timeout"] = *timeout
	}
	if provider.IsExternalProcessProvider(ctx, a.provider) {
		kwargs["command"], kwargs["args"] = a.acpCommand, a.acpArgs
	}
	if factory := hostDefaultHeadersFactory(baseURL); factory != nil {
		headers, err := factory(ctx, apiKey, baseURL)
		if err != nil {
			return nil, err
		}
		kwargs["default_headers"] = headers
	} else if routeHeaders := stringHeaders(provider.RouteAnswer(ctx, "client_headers", a.provider, a, map[string]any{"base_url": baseURL, "api_key": apiKey})); len(routeHeaders) > 0 {
		kwargs["default_headers"] = routeHeaders
	} else if profile, ok := provider.GetProviderProfile(ctx, a.provider); ok {
		if h := profile.DefaultHeaders(); len(h) > 0 {
			kwargs["default_headers"] = maps.Clone(h)
		}
	}
	return kwargs, nil
}

// Header map of a route answer or a kwargs value, nil when it holds none
func stringHeaders(v any) map[string]string {
	switch h := v.(type) {
	case map[string]string:
		if len(h) == 0 {
			return nil
		}
		return maps.Clone(h)
	case map[string]any:
		if len(h) == 0 {
			return nil
		}
		out := make(map[string]string, len(h))
		for k, val := range h {
			out[k] = fmt.Sprint(val)
		}
		return out
	}
	return nil
}

// Client kwargs through the provider router, then the init fallback chain, else the missing provider error
func routedClientKwargs(ctx context.Context, a *AIAgent, fallbackModel []map[string]any, timeout *float64) (map[string]any, error) {
	requested := a.provider
	if requested == "" {
		requested = "auto"
	}
	routed, _, err := provider.ResolveProviderClient(ctx, requested, a.model, provider.ProviderClientOptions{RawCodex: true})
	if err != nil {
		return nil, err
	}
	if routed != nil {
		if effective := provider.AuxClientEffectiveProvider(routed); provider.IsActualRoute(effective, "") {
			a.provider = provider.NormalizeProvider(effective)
		}
		return clientKwargsFromRouted(routed, timeout), nil
	}
	explicit := strings.ToLower(strings.TrimSpace(a.provider))
	var refused []string
	for _, fb := range fallbackEntries(fallbackModel) {
		fbProvider := fmt.Sprint(fb["provider"])
		fbModel := fmt.Sprint(fb["model"])
		baseURL, _ := fb["base_url"].(string)
		fbClient, resolvedModel, ferr := provider.ResolveProviderClient(ctx, fbProvider, fbModel, provider.ProviderClientOptions{
			RawCodex: true, ExplicitBaseURL: baseURL, ExplicitAPIKey: provider.ResolveFallbackEntryAPIKey(ctx, home.Dir(ctx), fb),
		})
		if ferr != nil {
			runAgentLog().Debug(fmt.Sprintf("Init-time fallback entry %s failed: %v", fbProvider, ferr))
			reason := ferr.Error()
			if reason == "" {
				reason = fmt.Sprintf("%T", ferr)
			}
			refused = append(refused, fmt.Sprintf("%s (%s)", fbProvider, reason))
			continue
		}
		if fbClient == nil {
			runAgentLog().Debug(fmt.Sprintf("Init-time fallback entry %s resolved no usable credentials", fbProvider))
			refused = append(refused, fbProvider+" (no usable credentials)")
			continue
		}
		a.fallbackActivated = true
		if strings.ToLower(strings.TrimSpace(fbProvider)) == "moa" {
			return nil, bindMoaRuntime(ctx, a, fbModel, nil)
		}
		a.provider, a.requestedProvider = fbProvider, fbProvider
		a.model = fbModel
		if resolvedModel != "" {
			a.model = resolvedModel
		}
		recomputeInitFallbackAPIMode(ctx, a, fbClient.BaseURL())
		return clientKwargsFromRouted(fbClient, timeout), nil
	}
	poolExhausted := false
	var pool *provider.Pool
	if explicit != "" && explicit != "auto" {
		func() {
			defer func() { _ = recover() }()
			pool = provider.LoadPool(agentPoolEnv(ctx), explicit)
			poolExhausted = pool != nil && pool.HasCredentials() && !pool.HasAvailable(a.model)
		}()
	}
	if len(refused) > 0 || poolExhausted {
		why := "no usable credentials"
		if poolExhausted {
			why = "credential pool exhausted"
		}
		entries := strings.Join(refused, "; ")
		if entries == "" {
			entries = "none configured"
		}
		runAgentLog().Warn(fmt.Sprintf("Init-time provider resolution failed: primary '%s' unresolvable (%s); fallback entries refused: %s", a.provider, why, entries))
	}
	if poolExhausted {
		message, ok := provider.PoolBillingMessage(ctx, explicit, a.model, pool)
		if !ok || message == "" {
			message, ok = provider.PoolCooldownMessage(ctx, explicit)
		}
		if ok && message != "" {
			return nil, &provider.ProviderCredentialsExhaustedError{Message: message, Provider: explicit}
		}
	}
	if explicit != "" && explicit != "auto" && explicit != "openrouter" && explicit != "custom" {
		return nil, &provider.ProviderNotConfiguredError{Message: provider.MissingProviderCredentialsMessage(ctx, explicit)}
	}
	return nil, &provider.ProviderNotConfiguredError{Message: "No LLM provider configured. Run `icebath model` to select a provider, or run `icebath setup` for first-time configuration."}
}

// OpenRouter Claude beta header, the model default headers, then the custom provider TLS and extra headers
func applyOpenAIHeaderPolicy(ctx context.Context, a *AIAgent, kwargs map[string]any) {
	effectiveBase := strings.ToLower(fmt.Sprint(kwargs["base_url"]))
	if matched, _ := pyurl.BaseURLHostMatches(effectiveBase, "openrouter.ai"); matched && strings.Contains(strings.ToLower(a.model), "claude") {
		headers := stringHeaders(kwargs["default_headers"])
		if headers == nil {
			headers = map[string]string{}
		}
		existing := headers["x-anthropic-beta"]
		if !strings.Contains(existing, provider.ToolStreamingBeta) {
			parts := []string{}
			if existing != "" {
				parts = append(parts, existing)
			}
			headers["x-anthropic-beta"] = strings.Join(append(parts, provider.ToolStreamingBeta), ",")
			kwargs["default_headers"] = headers
		}
	}
	a.applyUserDefaultHeaders(ctx)
	cfg := provider.LoadConfigSafe(home.Dir(ctx))
	entries := provider.CompatibleCustomProviders(cfg)
	cpBaseURL, _ := kwargs["base_url"].(string)
	if cpBaseURL == "" {
		cpBaseURL = a.baseURL
	}
	provider.ApplyCustomProviderTLSToClientKwargs(kwargs, cpBaseURL, entries, cfg)
	if extra := provider.CustomProviderExtraHeaders(cpBaseURL, entries, cfg); len(extra) > 0 {
		merged := stringHeaders(kwargs["default_headers"])
		if merged == nil {
			merged = map[string]string{}
		}
		maps.Copy(merged, extra)
		kwargs["default_headers"] = merged
	}
}

// Config model.default_headers merged onto the OpenAI client kwargs, user values winning, never on Anthropic or Bedrock
func (a *AIAgent) applyUserDefaultHeaders(ctx context.Context) {
	if a.apiMode == "anthropic_messages" || a.apiMode == "bedrock_converse" {
		return
	}
	if merged := provider.ApplyUserDefaultHeaders(ctx, stringHeaders(a.clientKwargs["default_headers"])); len(merged) > 0 {
		a.clientKwargs["default_headers"] = merged
	}
}

// OpenAI wire client from explicit or routed kwargs with the header and TLS policy applied
func initOpenAIClient(ctx context.Context, a *AIAgent, apiKey, baseURL string, fallbackModel []map[string]any, timeout *float64) error {
	var kwargs map[string]any
	if apiKey != "" && baseURL != "" {
		explicit, err := explicitClientKwargs(ctx, a, apiKey, baseURL, timeout)
		if err != nil {
			return err
		}
		kwargs = explicit
	} else {
		routed, err := routedClientKwargs(ctx, a, fallbackModel, timeout)
		if err != nil {
			return err
		}
		if routed == nil {
			if !a.quietMode {
				_, _ = fmt.Fprintf(statusStdout, "🤖 AI Agent initialized with MoA preset: %s\n", a.model)
			}
			return nil
		}
		kwargs = routed
	}
	kwBase, _ := kwargs["base_url"].(string)
	if provider.IsActualRoute(a.provider, kwBase) {
		a.apiMode = "chat_completions"
		a.transportCache = nil
	}
	if err := provider.ConfigureBedrockOpenAIClientKwargs(ctx, kwargs, timeout); err != nil {
		if a.provider == "bedrock" && strings.Contains(fmt.Sprint(kwargs["base_url"]), "bedrock-mantle.") {
			return err
		}
	}
	a.clientKwargs = kwargs
	applyOpenAIHeaderPolicy(ctx, a, kwargs)
	a.apiKey, _ = kwargs["api_key"].(string)
	if b, ok := kwargs["base_url"].(string); ok {
		a.setBaseURL(b)
	}
	client, err := a.createOpenAIClient(ctx, kwargs, "agent_init", true)
	if err != nil {
		return fmt.Errorf("Failed to initialize OpenAI client: %w", err)
	}
	a.client = client
	if !a.quietMode {
		_, _ = fmt.Fprintf(statusStdout, "🤖 AI Agent initialized with model: %s\n", a.model)
		if baseURL != "" {
			_, _ = fmt.Fprintf(statusStdout, "🔗 Using custom base URL: %s\n", baseURL)
		}
		var key any = "none"
		if k, present := kwargs["api_key"]; present {
			key = k
		}
		printKeyBanner(key, "API key", config.WarningNotificationsEnabled(ctx, a.platform, nil))
	}
	return nil
}

// Client for the agent's api mode, provider timeout resolved first
func buildClient(ctx context.Context, a *AIAgent, apiKey, baseURL string, fallbackModel []map[string]any) error {
	a.anthropicClient = nil
	a.isAnthropicOAuth = false
	var timeout *float64
	if t, ok := provider.GetProviderRequestTimeout(ctx, a.provider, a.model); ok {
		timeout = &t
	}
	switch {
	case a.apiMode == "anthropic_messages":
		return initAnthropicClient(ctx, a, apiKey, baseURL, timeout)
	case a.provider == "moa":
		return initMoaClient(ctx, a, apiKey)
	case a.apiMode == "bedrock_converse":
		return initBedrockClient(ctx, a, baseURL)
	}
	return initOpenAIClient(ctx, a, apiKey, baseURL, fallbackModel, timeout)
}

type hostHeadersFactory func(ctx context.Context, apiKey, baseURL string) (map[string]string, error)

type hostDefaultHeaders struct {
	host    string
	factory hostHeadersFactory
}

// Host matched default header factories for the routes whose servers require identifying headers
var hostDefaultHeadersTable = []hostDefaultHeaders{
	{"openrouter.ai", func(ctx context.Context, _, _ string) (map[string]string, error) {
		return provider.BuildORHeaders(ctx, nil)
	}},
	{"integrate.api.nvidia.com", func(_ context.Context, _, base string) (map[string]string, error) {
		return provider.BuildNvidiaNIMHeaders(base), nil
	}},
	{"api.routermint.com", func(ctx context.Context, _, _ string) (map[string]string, error) { return routermintHeaders() }},
	{"api.kimi.com", func(context.Context, string, string) (map[string]string, error) {
		return map[string]string{"User-Agent": "claude-code/0.1.0"}, nil
	}},
	{"portal.qwen.ai", func(context.Context, string, string) (map[string]string, error) { return qwenPortalHeaders(), nil }},
	{"chatgpt.com", func(_ context.Context, key, base string) (map[string]string, error) {
		return provider.CodexCloudflareHeaders(key, base), nil
	}},
	{"x.ai", func(context.Context, string, string) (map[string]string, error) {
		return tools.XAIDefaultHeaders(), nil
	}},
}

// Header factory of the first table host the base url matches, nil when none does
func hostDefaultHeadersFactory(baseURL string) hostHeadersFactory {
	for _, entry := range hostDefaultHeadersTable {
		if matched, _ := pyurl.BaseURLHostMatches(baseURL, entry.host); matched {
			return entry.factory
		}
	}
	return nil
}

// Client kwargs mirroring a router resolved client with its provider headers
func clientKwargsFromRouted(client provider.AuxClient, timeout *float64) map[string]any {
	kwargs := map[string]any{"api_key": client.APIKey(), "base_url": client.BaseURL()}
	if timeout != nil {
		kwargs["timeout"] = *timeout
	}
	if headers := provider.AuxClientCustomHeaders(client); len(headers) > 0 {
		kwargs["default_headers"] = headers
	}
	return kwargs
}
