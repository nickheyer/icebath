package provider

import (
	"context"
	"net/http"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Options of one OpenAI SDK client beyond its key and base URL
type auxOpenAIOptions struct {
	DefaultHeaders map[string]string
	DefaultQuery   map[string]string
	Timeout        float64
	MaxRetries     *int
	HTTPClient     *http.Client
}

// OpenAIClientOptions are the options of one OpenAI SDK client beyond its key and base URL
type OpenAIClientOptions = auxOpenAIOptions

// Keepalive client builder seam, nil when the client cannot be built and the SDK default applies
var auxBuildKeepaliveHTTPClientFn = buildKeepaliveHTTPClient

// TLS verification for an auxiliary base URL mirroring the main client, any config failure giving the default
func auxResolveAuxVerify(ctx context.Context, baseURL string) TLSVerify {
	tree, err := config.LoadReadonly(ctx, home.Dir(ctx), nil)
	if err != nil {
		return TLSVerify{}
	}
	tls := CustomProviderTLSSettings(baseURL, nil, map[string]any(tree))
	var sslVerify any
	if tls.HasVerify {
		sslVerify = tls.Verify
	}
	return ResolveTLSVerify(nil, tls.CACert, sslVerify, baseURL)
}

// Keepalive HTTP client with the environment only proxy policy, nil leaving the SDK default client
func auxOpenAIHTTPClient(ctx context.Context, baseURL string, asyncMode bool) *http.Client {
	return auxBuildKeepaliveHTTPClientFn(baseURL, asyncMode, auxResolveAuxVerify(ctx, baseURL))
}

// OpenAI SDK client for an auxiliary route with SDK retries off, the probe client inside an availability probe
func auxCreateOpenAIClient(ctx context.Context, apiKey any, baseURL string, opts auxOpenAIOptions) AuxClient {
	staticKey, _ := apiKey.(string)
	if auxProbeActive(ctx) {
		return &auxProbeClient{apiKey: staticKey, baseURL: baseURL}
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = auxOpenAIHTTPClient(ctx, baseURL, false)
	}
	opts.DefaultHeaders = applyRequiredCodexHeaders(opts.DefaultHeaders, staticKey, baseURL)
	if opts.MaxRetries == nil {
		noRetries := 0
		opts.MaxRetries = &noRetries
	}
	return auxOpenAIConstructorFn(apiKey, baseURL, opts)
}

// OpenAI SDK constructor of the auxiliary routes, the class the hermes tests patch
var auxOpenAIConstructorFn = newAuxOpenAIClient
