package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
)

// InvokeModel body version the AnthropicBedrock client stamps on every Messages body
const bedrockAnthropicVersion = "bedrock-2023-05-31"

// Anthropic Messages wire over Bedrock InvokeModel, the AnthropicBedrock SDK client
type anthBedrockTransport struct {
	route Route
}

func bedrockTransport(route Route) (anthTransport, error) {
	return &anthBedrockTransport{route: route}, nil
}

// Route region, then the bedrock-runtime host region, then the configured runtime region
func (t *anthBedrockTransport) region(ctx context.Context) string {
	if r := strings.TrimSpace(t.route.Region); r != "" {
		return r
	}
	if isBedrockRuntimeEndpoint(t.route.BaseURL) {
		return bedrockRegionFromRuntimeURL(t.route.BaseURL)
	}
	return resolveBedrockRuntimeRegionCtx(ctx)
}

// ANTHROPIC_BEDROCK_BASE_URL read through the routed profile's scope under a home override
func bedrockAnthropicBaseURLOverride(ctx context.Context) string {
	if home.Override(ctx) != "" {
		return strings.TrimSpace(GetSecretStr(ctx, "ANTHROPIC_BEDROCK_BASE_URL", ""))
	}
	return strings.TrimSpace(os.Getenv("ANTHROPIC_BEDROCK_BASE_URL"))
}

// Reports an api key that is a real Bedrock bearer token rather than the aws-sdk sentinel
func bedrockRealAPIKey(key string) bool {
	key = strings.TrimSpace(key)
	return key != "" && key != "aws-sdk" && key != "no-key-required"
}

// Header lookup ignoring case, the way HTTP header names compare
func bedrockHeaderValue(headers map[string]string, name string) (string, bool) {
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}

// InvokeModel form of a Messages body, model and stream lifted out and the betas moved into the body
func bedrockInvokeBody(body map[string]any, headers map[string]string) (map[string]any, string, bool) {
	inner := make(map[string]any, len(body)+2)
	for k, v := range body {
		inner[k] = v
	}
	model := stringOf(inner["model"])
	stream, _ := inner["stream"].(bool)
	delete(inner, "model")
	delete(inner, "stream")
	if _, has := inner["anthropic_version"]; !has {
		inner["anthropic_version"] = bedrockAnthropicVersion
	}
	if betas, ok := bedrockHeaderValue(headers, "anthropic-beta"); ok && betas != "" {
		if _, has := inner["anthropic_beta"]; !has {
			inner["anthropic_beta"] = strings.Split(betas, ",")
		}
	}
	return inner, model, stream
}

// Request posts the InvokeModel, InvokeModelWithResponseStream or CountTokens call, bearer or SigV4 signed
func (t *anthBedrockTransport) Request(ctx context.Context, op anthOperation, body map[string]any, headers map[string]string) (*http.Request, error) {
	inner, model, stream := bedrockInvokeBody(body, headers)
	if model == "" {
		model = t.route.Model
	}
	region := t.region(ctx)
	client, err := bedrockRuntimeClient(ctx, region, model)
	if err != nil {
		return nil, err
	}
	operation := "invoke"
	var payload any = inner
	switch {
	case op == anthOpCountTokens:
		operation = "count-tokens"
		if _, has := inner["max_tokens"]; !has {
			inner["max_tokens"] = 1
		}
		encoded, err := json.Marshal(inner)
		if err != nil {
			return nil, fmt.Errorf("provider: encode bedrock count tokens body: %w", err)
		}
		payload = map[string]any{"input": map[string]any{"invokeModel": map[string]any{"body": encoded}}}
	case stream:
		operation = "invoke-with-response-stream"
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("provider: encode bedrock invoke body: %w", err)
	}
	endpoint := bedrockAnthropicBaseURLOverride(ctx)
	if endpoint == "" {
		endpoint = client.endpoint
	}
	target := strings.TrimRight(endpoint, "/") + bedrockModelPath(model, operation)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		if strings.EqualFold(k, "anthropic-version") {
			continue
		}
		req.Header.Set(k, v)
	}
	for k, v := range bedrockGuardrailHeaders(bedrockProcessConfig(ctx)) {
		if _, has := bedrockHeaderValue(headers, k); !has {
			req.Header.Set(k, v)
		}
	}
	return req, t.authorize(ctx, req, data, client, region)
}

// Signs with the route key as bearer, else the route's static keys, else the client's bearer or credential chain
func (t *anthBedrockTransport) authorize(ctx context.Context, req *http.Request, data []byte, client *awsServiceClient, region string) error {
	if bedrockRealAPIKey(t.route.APIKey) {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(t.route.APIKey))
		return nil
	}
	if t.route.AccessKeyID != "" || t.route.SecretAccessKey != "" {
		creds, err := awsStaticProvider{creds: awsCredentials{AccessKeyID: t.route.AccessKeyID, SecretAccessKey: t.route.SecretAccessKey,
			SessionToken: t.route.SessionToken, Source: "route"}}.Retrieve(ctx)
		if err != nil {
			return err
		}
		_, err = sigV4Sign(req, data, creds, "bedrock", region, awsNow())
		return err
	}
	return client.authorize(ctx, req, data)
}

// Events decodes the AWS event stream chunks into Anthropic stream events
func (t *anthBedrockTransport) Events(resp *http.Response) anthEventSource {
	return newBedrockInvokeEvents(resp.Body, resp.Header.Clone())
}

// StreamUnavailable adds IAM denying InvokeModelWithResponseStream to the direct transport signals
func (t *anthBedrockTransport) StreamUnavailable(err error) bool {
	return (&anthDirectTransport{}).StreamUnavailable(err) || isBedrockStreamingAccessDenied(err)
}

// GuardrailIntervened reads the guardrail action Bedrock adds to a substituted reply
func (t *anthBedrockTransport) GuardrailIntervened(message map[string]any) bool {
	return anthropicResponseGuardrailIntervened(message)
}

// ResolveProfileARN names the foundation model behind an application inference profile ARN under the launch identity
func (t *anthBedrockTransport) ResolveProfileARN(arn string) string {
	return t.resolveProfileARN(context.Background(), arn)
}

// Foundation model of a profile ARN resolved with the credentials of the home the context serves
func (t *anthBedrockTransport) resolveProfileARN(ctx context.Context, arn string) string {
	return resolveInferenceProfileModelID(ctx, arn, t.region(ctx))
}

// Profile ARN resolver bound to the request context so a routed profile resolves with its own identity
func anthProfileResolver(ctx context.Context, t anthTransport) func(string) string {
	if b, ok := t.(*anthBedrockTransport); ok {
		return func(arn string) string { return b.resolveProfileARN(ctx, arn) }
	}
	return t.ResolveProfileARN
}

// Betas are the common betas plus the 1M context beta Bedrock gates the long window behind
func (t *anthBedrockTransport) Betas(dropContext1M bool) []string {
	betas := cloneStrings(anthropicCommonBetas)
	if !dropContext1M {
		betas = append(betas, anthropicContext1MBeta)
	}
	return betas
}
