package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"
)

// Keys of the provider registry of the bound home in storage order, alias keys included
func RegistryKeys(ctx context.Context) []string {
	if ctx == nil {
		ctx = context.Background()
	}
	items := providerRegistry().items(ctx)
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Key)
	}
	return out
}

// Registry row of the bound home stored under one key, nil when the key is absent
func RegistryRow(ctx context.Context, key string) *Profile {
	if ctx == nil {
		ctx = context.Background()
	}
	return providerRegistry().get(ctx, key)
}

// Key env names then the base URL env name of a profile, the order its env_vars tuple lists them
func ProfileEnvVars(p Profile) []string { return profileEnvVars(p) }

// Comma joined anthropic-beta values an OAuth token probe sends, with and without the 1M context beta
func AnthropicOAuthProbeBetas() (full, withoutContext1M string) {
	var reduced []string
	for _, b := range anthropicCommonBetas {
		if b != anthropicContext1MBeta {
			reduced = append(reduced, b)
		}
	}
	full = strings.Join(append(append([]string{}, anthropicCommonBetas...), anthropicOAuthOnlyBetas...), ",")
	return full, strings.Join(append(reduced, anthropicOAuthOnlyBetas...), ",")
}

// Normalized Anthropic base URL with the default query Azure endpoints need, nil for every other endpoint
func AnthropicBaseClientQuery(baseURL string) (string, map[string]string) {
	normalized, kwargs := anthBaseClientKwargs(baseURL, 0)
	return normalized, kwargs.defaultQuery
}

// Whether an Anthropic compatible base URL authenticates with a bearer token instead of x-api-key
func AnthropicRequiresBearerAuth(baseURL string) bool { return requiresBearerAuth(baseURL) }

// Whether an aiplatform base URL speaks the native Gemini API with an API key
func IsVertexExpressBaseURL(normalized string) bool { return isVertexExpressBaseURL(normalized) }

// Number of foundation models the Bedrock control plane lists for a region, one attempt with a ten second read limit
func BedrockFoundationModelCount(ctx context.Context, region string) (int, error) {
	opts, err := scopedAWSSessionOptions(ctx)
	if err != nil {
		return 0, err
	}
	c := newAWSServiceClient(awsClientConfig{service: "bedrock", signingName: "bedrock", region: region, session: opts,
		bearer: resolveBedrockBearerTokenCtx(ctx), readTimeout: 10 * time.Second, totalMaxAttempts: 1})
	var out struct {
		ModelSummaries []json.RawMessage `json:"modelSummaries"`
	}
	if err := c.doJSON(ctx, "ListFoundationModels", http.MethodGet, "/foundation-models", nil, nil, &out); err != nil {
		return 0, err
	}
	return len(out.ModelSummaries), nil
}

// Name of the botocore exception class an AWS client error stands for
func AWSErrorClassName(err error) string {
	var service *AWSServiceError
	var readTimeout *AWSReadTimeoutError
	var credential *AWSCredentialError
	var network net.Error
	switch {
	case errors.As(err, &service):
		return "ClientError"
	case errors.As(err, &readTimeout):
		return "ReadTimeoutError"
	case errors.Is(err, ErrNoAWSCredentials):
		return "NoCredentialsError"
	case errors.As(err, &credential):
		return "CredentialRetrievalError"
	case errors.As(err, &network):
		return "EndpointConnectionError"
	}
	return auxPyExceptionName(err)
}

// Sets whether kept notices are served on stderr and returns the previous setting, for tests that restore it
func SwapNoticesOnStderrForTests(on bool) bool { return leftCoreTerminalFrontEnd.Swap(on) }
