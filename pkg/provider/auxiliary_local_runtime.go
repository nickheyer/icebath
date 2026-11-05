package provider

import (
	"context"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Base URL and key of the profile's local llama.cpp server for a bare alias, ok false when the request is not one
func auxBareLlamacppEndpoint(ctx context.Context, provider, baseURL, apiKey string) (string, string, bool, error) {
	alias := strings.ToLower(strings.TrimSpace(provider))
	if !LlamacppAliases[alias] || baseURL != "" {
		return "", "", false, nil
	}
	h := home.Dir(ctx)
	if namedCustomProviderConfigured(ctx, alias, h) {
		return "", "", false, nil
	}
	endpoint, err := ResolveLlamacppEndpoint(ctx, nil, h, 8*time.Second)
	if err != nil {
		return "", "", false, err
	}
	if endpoint == nil {
		endpoint = &LlamacppEndpoint{}
	}
	key := apiKey
	if key == "" {
		key = endpoint.APIKey
	}
	if key == "" {
		key = "no-key-required"
	}
	return strings.TrimSpace(endpoint.BaseURL), key, true, nil
}
