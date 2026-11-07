package agent

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// OpenAI SDK constructor seam, the process_bootstrap.OpenAI tests patch
var openAIClientConstructor = openAIClientFromKwargs

// Text of a kwargs value as str(value or "") reads it
func kwargText(v any) string {
	if !pyTruthy(v) {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// Seconds of a numeric kwargs timeout, false for None or a non numeric value
func kwargSeconds(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	}
	return 0, false
}

// OpenAI SDK client from constructor keyword arguments, an unknown keyword raising the constructor TypeError
func openAIClientFromKwargs(kwargs map[string]any) (any, error) {
	var opts provider.OpenAIClientOptions
	var apiKey any
	headers := map[string]string{}
	for key, value := range kwargs {
		switch key {
		case "api_key":
			apiKey = value
		case "base_url", "websocket_base_url", "webhook_secret", "_strict_response_validation":
		case "default_headers":
			maps.Copy(headers, stringHeaders(value))
		case "default_query":
			opts.DefaultQuery = stringHeaders(value)
		case "timeout":
			if value != nil {
				seconds, ok := kwargSeconds(value)
				if !ok {
					return nil, fmt.Errorf("TypeError: invalid timeout %s", pyrepr.Repr(value))
				}
				opts.Timeout = seconds
			}
		case "max_retries":
			n, ok := value.(int)
			if !ok {
				return nil, fmt.Errorf("TypeError: max_retries must be an int, got %s", pyrepr.Repr(value))
			}
			opts.MaxRetries = &n
		case "http_client":
			if value != nil {
				hc, ok := value.(*http.Client)
				if !ok {
					return nil, fmt.Errorf("TypeError: Invalid `http_client` argument; Expected an instance of `httpx.Client` but got %T", value)
				}
				opts.HTTPClient = hc
			}
		case "organization", "project":
			name := map[string]string{"organization": "OpenAI-Organization", "project": "OpenAI-Project"}[key]
			if s, ok := value.(string); ok && s != "" {
				if _, set := stringHeaders(kwargs["default_headers"])[name]; !set {
					headers[name] = s
				}
			}
		default:
			return nil, fmt.Errorf("TypeError: OpenAI.__init__() got an unexpected keyword argument '%s'", key)
		}
	}
	if len(headers) > 0 {
		opts.DefaultHeaders = headers
	}
	return provider.NewOpenAIClient(apiKey, kwargText(kwargs["base_url"]), opts), nil
}

// Custom client from the registered provider profile by name then base url prefix, nil when none supplies one
func providerSuppliedClient(ctx context.Context, a *AIAgent, clientKwargs map[string]any) (client any) {
	providerName := strings.TrimSpace(a.provider)
	var profile *provider.Profile
	if providerName != "" {
		if p, ok := provider.GetProviderProfile(ctx, providerName); ok {
			profile = &p
		}
	}
	if profile == nil {
		if baseURL := strings.TrimSpace(kwargText(clientKwargs["base_url"])); baseURL != "" {
			profile = profileForBaseURL(ctx, baseURL)
		}
	}
	if profile == nil {
		return nil
	}
	warn := func(detail any) {
		name := profile.Name
		if name == "" {
			name = providerName
		}
		if name == "" {
			name = "?"
		}
		runAgentLog().Warn(fmt.Sprintf("Provider profile %s failed to create a client; falling back to the standard client path", pyrepr.Repr(name)), "error", detail)
	}
	defer func() {
		if r := recover(); r != nil {
			warn(r)
			client = nil
		}
	}()
	created, err := profile.CreateClient(ctx, clientKwargs)
	if err != nil {
		warn(err)
		return nil
	}
	if created == nil {
		return nil
	}
	return created
}

// Registered profile whose own base url is the given url or a path prefix of it
func profileForBaseURL(ctx context.Context, baseURL string) *provider.Profile {
	target := strings.ToLower(strings.TrimRight(baseURL, "/"))
	for _, candidate := range provider.ListProviders(ctx) {
		own := strings.ToLower(strings.TrimRight(candidate.BaseURL, "/"))
		if own != "" && (target == own || strings.HasPrefix(target, own+"/")) {
			return &candidate
		}
	}
	return nil
}

// Adds the route's required client headers the kwargs lack, never overriding a present key
func ensureRouteClientHeaders(ctx context.Context, clientKwargs map[string]any, providerName string) {
	required := stringHeaders(provider.RouteAnswer(ctx, "client_headers", providerName, nil, map[string]any{
		"base_url": kwargText(clientKwargs["base_url"]), "api_key": kwargText(clientKwargs["api_key"]),
	}))
	if len(required) == 0 {
		return
	}
	existing := stringHeaders(clientKwargs["default_headers"])
	if existing == nil {
		existing = map[string]string{}
	}
	lower := map[string]bool{}
	for k := range existing {
		lower[strings.ToLower(k)] = true
	}
	for k, v := range required {
		if !lower[strings.ToLower(k)] {
			existing[k] = v
		}
	}
	clientKwargs["default_headers"] = existing
}

// Native Gemini client when the base url is the Gemini API, nil otherwise
func geminiNativeClient(a *AIAgent, clientKwargs map[string]any, verify provider.TLSVerify, reason string, shared bool) (any, error) {
	baseURL := kwargText(clientKwargs["base_url"])
	if !provider.IsNativeGeminiBaseURL(baseURL) {
		return nil, nil
	}
	route := provider.Route{BaseURL: baseURL, APIKey: kwargText(clientKwargs["api_key"]), Headers: stringHeaders(clientKwargs["default_headers"])}
	if seconds, ok := kwargSeconds(clientKwargs["timeout"]); ok {
		route.Timeout = time.Duration(seconds * float64(time.Second))
	}
	hc, _ := clientKwargs["http_client"].(*http.Client)
	if _, present := clientKwargs["http_client"]; !present {
		hc = a.buildKeepaliveHTTPClient(baseURL, verify)
	}
	if hc != nil {
		route.Transport = hc.Transport
	}
	client, err := provider.NewGeminiNativeClient(route)
	if err != nil {
		return nil, err
	}
	runAgentLog().Info(fmt.Sprintf("Gemini native client created (%s, shared=%s) %s", reason, pyBoolStr(shared), a.ClientLogContext()))
	return client, nil
}

// Wire client for the kwargs copy: MoA facade, profile client, native Gemini, else the keepalive OpenAI client
func (a *AIAgent) createOpenAIClient(ctx context.Context, clientKwargs map[string]any, reason string, shared bool) (any, error) {
	kwargs := maps.Clone(clientKwargs)
	if kwargs == nil {
		kwargs = map[string]any{}
	}
	if pyTruthy(kwargs["base_url"]) {
		kwargs["base_url"] = provider.ToOpenAIBaseURL(kwargText(kwargs["base_url"]))
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				runAgentLog().Debug("Provider client-kwargs hook skipped", "error", r)
			}
		}()
		if profile, ok := provider.GetProviderProfile(ctx, a.provider); ok {
			for key, value := range profile.BuildClientKwargsExtras(map[string]any{"base_url": pyOr(kwargs["base_url"], "")}) {
				if _, set := kwargs[key]; !set {
					kwargs[key] = value
				}
			}
		}
	}()
	if strings.ToLower(strings.TrimSpace(a.provider)) == "moa" {
		return buildMoaFacade(ctx, a, pyOr(a.model, "default")), nil
	}
	sslCACert := kwargText(kwargs["ssl_ca_cert"])
	sslVerify := kwargs["ssl_verify"]
	delete(kwargs, "ssl_ca_cert")
	delete(kwargs, "ssl_verify")
	verify := provider.ResolveTLSVerify(runAgentLog(), sslCACert, sslVerify, kwargText(kwargs["base_url"]))
	if err := provider.ValidateProxyEnvURLs(); err != nil {
		return nil, err
	}
	if err := provider.ValidateBaseURL(kwargText(kwargs["base_url"])); err != nil {
		return nil, err
	}
	if client := providerSuppliedClient(ctx, a, kwargs); client != nil {
		runAgentLog().Info(fmt.Sprintf("%s client created from provider profile (%s, shared=%s) %s", a.provider, reason, pyBoolStr(shared), a.ClientLogContext()))
		return client, nil
	}
	if provider.IsGeminiNativeProviderName(a.provider) {
		client, err := geminiNativeClient(a, kwargs, verify, reason, shared)
		if err != nil || client != nil {
			return client, err
		}
	}
	if strings.Contains(kwargText(kwargs["base_url"]), "bedrock-mantle.") {
		var timeout *float64
		if seconds, ok := kwargSeconds(kwargs["timeout"]); ok {
			timeout = &seconds
		}
		if err := provider.ConfigureBedrockOpenAIClientKwargs(ctx, kwargs, timeout); err != nil {
			return nil, err
		}
	}
	if _, present := kwargs["http_client"]; !present {
		if hc := a.buildKeepaliveHTTPClient(kwargText(kwargs["base_url"]), verify); hc != nil {
			kwargs["http_client"] = hc
		}
	}
	if _, set := kwargs["max_retries"]; !set {
		kwargs["max_retries"] = 0
	}
	ensureRouteClientHeaders(ctx, kwargs, a.provider)
	accessToken, _ := kwargs["api_key"].(string)
	if headers := provider.ApplyRequiredCodexHeaders(stringHeaders(kwargs["default_headers"]), accessToken, kwargText(kwargs["base_url"])); len(headers) > 0 {
		kwargs["default_headers"] = headers
	}
	client, err := openAIClientConstructor(kwargs)
	if err != nil {
		return nil, err
	}
	installServedModelCapture(a, client)
	runAgentLog().Info(fmt.Sprintf("OpenAI client created (%s, shared=%s) %s", reason, pyBoolStr(shared), a.ClientLogContext()))
	return client, nil
}
