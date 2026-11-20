package provider

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

// Hosts that accept an Azure api-key header beside a bearer token
var azureAPIHostSuffixes = []string{"openai.azure.com", "cognitiveservices.azure.com", "services.ai.azure.com"}

// Header names a minted bearer token replaces on Azure routes
var azureStrippedAuthHeaders = []string{"Authorization", "Api-Key", "X-Api-Key"}

// Reports whether the base URL points at an Azure OpenAI or Foundry host
func isAzureAPIHost(baseURL string) bool {
	for _, h := range azureAPIHostSuffixes {
		if hostMatches(baseURL, h) {
			return true
		}
	}
	return false
}

// Base URL without its query string, plus the query every request must carry
func ccSplitBaseURLQuery(raw string) (string, url.Values) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.RawQuery == "" {
		return strings.TrimSpace(raw), url.Values{}
	}
	query := url.Values{}
	for k, vals := range u.Query() {
		if len(vals) > 0 {
			query.Set(k, vals[0])
		}
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), query
}

// Endpoint URL for a wire path, with Azure deployment paths and api-version applied
func ccEndpointURL(route Route, path string) (string, error) {
	base, query := ccSplitBaseURLQuery(route.BaseURL)
	base = strings.TrimRight(base, "/")
	if base == "" {
		return "", errors.New("provider: route has no base URL")
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", errors.New("provider: base URL must be absolute: " + route.BaseURL)
	}
	if dep := strings.TrimSpace(route.Deployment); dep != "" && hostMatches(base, "openai.azure.com") && !strings.Contains(u.Path, "/deployments/") {
		u.Path = "/openai/deployments/" + url.PathEscape(dep)
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	if v := strings.TrimSpace(route.APIVersion); v != "" && query.Get("api-version") == "" {
		query.Set("api-version", v)
	}
	u.RawQuery = query.Encode()
	return u.String(), nil
}

// Auth headers of one request, refreshing a token source per call, plus names to strip first
func ccAuthHeaders(ctx context.Context, route Route) (map[string]string, []string, error) {
	headers := map[string]string{}
	azure := isAzureAPIHost(route.BaseURL)
	if route.TokenSource != nil {
		token, err := route.TokenSource(ctx)
		if err != nil {
			return nil, nil, err
		}
		if strings.TrimSpace(token) == "" {
			return headers, azureStrippedAuthHeaders, nil
		}
		headers["Authorization"] = "Bearer " + strings.TrimSpace(token)
		return headers, azureStrippedAuthHeaders, nil
	}
	key := strings.TrimSpace(route.APIKey)
	if key == "" {
		return headers, nil, nil
	}
	headers["Authorization"] = "Bearer " + key
	if azure {
		headers["api-key"] = key
	}
	return headers, nil, nil
}
