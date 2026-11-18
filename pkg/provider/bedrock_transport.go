package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
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

// InvokeModel form of a Messages body as AnthropicBedrock prepares it, version and betas set by default, model and stream lifted out, extra_body merged last
func bedrockInvokeBody(body map[string]any, op anthOperation, headers map[string]string) (*pyjson.Object, string, bool, error) {
	params, extra, err := anthWireParams(body, op)
	if err != nil {
		return nil, "", false, err
	}
	inner := params.Copy()
	if _, has := inner.Get("anthropic_version"); !has {
		inner.Set("anthropic_version", bedrockAnthropicVersion)
	}
	if betas, ok := bedrockHeaderValue(headers, "anthropic-beta"); ok && betas != "" {
		if _, has := inner.Get("anthropic_beta"); !has {
			list := []any{}
			for _, b := range strings.Split(betas, ",") {
				list = append(list, b)
			}
			inner.Set("anthropic_beta", list)
		}
	}
	modelValue, _ := inner.Pop("model")
	streamValue, _ := inner.Pop("stream")
	stream, _ := streamValue.(bool)
	return anthMergedWireBody(inner, extra), stringOf(modelValue), stream, nil
}

// Request posts the InvokeModel, InvokeModelWithResponseStream or CountTokens call under the AnthropicBedrock head, bearer or SigV4 signed
func (t *anthBedrockTransport) Request(ctx context.Context, op anthOperation, body map[string]any, head anthHead, call anthSDKCall) (*http.Request, error) {
	callBetas := map[string]string{}
	if betas, ok := head.extra.get("anthropic-beta"); ok {
		callBetas["anthropic-beta"] = betas
	}
	inner, model, stream, err := bedrockInvokeBody(body, op, callBetas)
	if err != nil {
		return nil, err
	}
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
		if _, has := inner.Get("max_tokens"); !has {
			inner.Set("max_tokens", 1)
		}
		encoded, err := pyhttpx.JSONBody(inner)
		if err != nil {
			return nil, fmt.Errorf("provider: encode bedrock count tokens body: %w", err)
		}
		payload = pyjson.ObjectOf("input", pyjson.ObjectOf("invokeModel", pyjson.ObjectOf("body", base64.StdEncoding.EncodeToString(encoded))))
	case stream:
		operation = "invoke-with-response-stream"
	}
	data, err := pyhttpx.JSONBody(payload)
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
	lines, err := head.lines(stream && op == anthOpMessages, call)
	if err != nil {
		return nil, err
	}
	req.Host = httpxHost(req.URL)
	signed, err := t.authorize(ctx, req, httpxPreparedLines(req, lines, len(data)), data, client, region)
	if err != nil {
		return nil, err
	}
	return pyhttpx.WithWire(req, signed), nil
}

// Lines the _prepare_request hook leaves, the route key or client bearer set as Authorization, else botocore SigV4 over the route keys or credential chain
func (t *anthBedrockTransport) authorize(ctx context.Context, req *http.Request, lines *pyhttpx.Headers, data []byte, client *awsServiceClient, region string) (*pyhttpx.Headers, error) {
	bearer := ""
	switch {
	case bedrockRealAPIKey(t.route.APIKey):
		bearer = strings.TrimSpace(t.route.APIKey)
	case t.route.AccessKeyID == "" && t.route.SecretAccessKey == "":
		bearer = client.bearer
	}
	if bearer != "" {
		out := pyhttpx.NewHeaders(lines.Lines()...)
		out.Set("Authorization", "Bearer "+bearer)
		return out, nil
	}
	var creds awsCredentials
	var err error
	if t.route.AccessKeyID != "" || t.route.SecretAccessKey != "" {
		creds, err = awsStaticProvider{creds: awsCredentials{AccessKeyID: t.route.AccessKeyID, SecretAccessKey: t.route.SecretAccessKey,
			SessionToken: t.route.SessionToken, Source: "route"}}.Retrieve(ctx)
	} else {
		creds, err = client.creds.Retrieve(ctx)
	}
	if err != nil {
		return nil, err
	}
	return botocoreSignedLines(req, lines, data, creds, "bedrock", region, awsNow())
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

// Frame source the SDK stream readers consume, SSE frames of a body or the chunk events of an AWS event stream
type sseFrameSource interface {
	Next() (*sseFrame, error)
}

// Frames the SDK reads from a streamed response, the AWSEventStreamDecoder of the AnthropicBedrock client on Bedrock
func (c *anthropicClient) sseFrames(resp *http.Response) sseFrameSource {
	if c.variant == "bedrock" {
		return &bedrockSSEFrames{r: newAWSEventStreamReader(resp.Body)}
	}
	return newSSEReader(resp.Body)
}

// SSE frames AWSEventStreamDecoder yields, one per chunk event named by its type, error frames raised as its ValueError
type bedrockSSEFrames struct{ r *awsEventStreamReader }

func (s *bedrockSSEFrames) Next() (*sseFrame, error) {
	for {
		m, err := s.r.Next()
		if err != nil {
			return nil, err
		}
		if kind := m.header(":message-type"); kind == "error" || kind == "exception" {
			return nil, &ValueError{Message: "Bad response code, expected 200: " + awsResponseDictRepr(m, 400)}
		}
		if m.header(":event-type") != "chunk" {
			continue
		}
		var chunk struct {
			Bytes *string `json:"bytes"`
		}
		if json.Unmarshal(m.Payload, &chunk) != nil || chunk.Bytes == nil || *chunk.Bytes == "" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(*chunk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("binascii.Error: %w", err)
		}
		if !utf8.Valid(raw) {
			return nil, fmt.Errorf("UnicodeDecodeError: 'utf-8' codec can't decode the chunk bytes")
		}
		event := "completion"
		var data any
		if json.Unmarshal(raw, &data) == nil {
			if payload, ok := data.(map[string]any); ok {
				if t, ok := payload["type"].(string); ok {
					event = t
				}
			}
		}
		return &sseFrame{Event: event, Data: string(raw)}, nil
	}
}

// Python repr of the response dict botocore EventStreamMessage.to_response_dict builds for a frame
func awsResponseDictRepr(m *awsEventStreamMessage, status int) string {
	var headers []string
	for _, h := range m.Headers {
		headers = append(headers, pyrepr.StrRepr(h.Name)+": "+pyrepr.Repr(h.Value))
	}
	return fmt.Sprintf("{'status_code': %d, 'headers': {%s}, 'body': %s}", status, strings.Join(headers, ", "), pyrepr.BytesRepr(m.Payload))
}
