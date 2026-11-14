package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Anthropic SDK style client over the Messages wire, taking raw api kwargs and returning raw Message maps
type anthropicSDKClient struct {
	inner     *anthropicClient
	baseURL   string
	betas     []string
	transport *http.Transport
	closed    atomic.Bool
	apiKey    string
	authToken string
}

var _ interface {
	APIKey() string
	AuthToken() string
} = (*anthropicSDKClient)(nil)

// SDK api_key attribute, the key passed as api_key or its environment default
func (c *anthropicSDKClient) APIKey() string { return c.apiKey }

// SDK auth_token attribute, the bearer or OAuth key passed as auth_token or its environment default
func (c *anthropicSDKClient) AuthToken() string { return c.authToken }

// SDK api_key and auth_token attributes of a direct client, the unpassed one read from its environment variable
func anthSDKKeyAttributes(apiKey any, baseURL string) (string, string) {
	if _, ok := anthSDKTokenSource(apiKey); ok {
		return os.Getenv("ANTHROPIC_API_KEY"), "entra-id-bearer-via-http-hook"
	}
	key, isStr := apiKey.(string)
	switch anthAuthStyle(key, baseURL, anthNormalizedBaseURL(baseURL)) {
	case "bearer", "oauth":
		if !isStr {
			key = os.Getenv("ANTHROPIC_AUTH_TOKEN")
		}
		return os.Getenv("ANTHROPIC_API_KEY"), key
	}
	if !isStr {
		key = os.Getenv("ANTHROPIC_API_KEY")
	}
	return key, os.Getenv("ANTHROPIC_AUTH_TOKEN")
}

// Normalized SDK base URL the client posts to
func (c *anthropicSDKClient) BaseURL() string { return c.baseURL }

// Whether the client was closed
func (c *anthropicSDKClient) IsClosed() bool { return c.closed.Load() }

// Marks the client closed and drops its pooled connections
func (c *anthropicSDKClient) Close() error {
	c.closed.Store(true)
	c.transport.CloseIdleConnections()
	return nil
}

// Bearer token source of a callable key, false for a static key
func anthSDKTokenSource(apiKey any) (func(context.Context) (string, error), bool) {
	switch k := apiKey.(type) {
	case *TaggedTokenSource:
		if k != nil {
			return k.Func(), true
		}
	case func(context.Context) (string, error):
		return k, true
	case func() (string, error):
		return func(context.Context) (string, error) { return k() }, true
	case func() string:
		return func(context.Context) (string, error) { return k(), nil }, true
	}
	return nil, false
}

// Anthropic client for a static key or bearer callable, 10s connect and the read timeout or 900s
func buildAnthropicClient(ctx context.Context, apiKey any, baseURL string, timeout float64, dropContext1MBeta bool) (*anthropicSDKClient, error) {
	route := Route{Provider: "anthropic", Mode: ModeAnthropicMessages, BaseURL: baseURL}
	if src, ok := anthSDKTokenSource(apiKey); ok {
		route.TokenSource = src
	} else {
		config.NormalizeProxyEnvVars()
		key, _ := apiKey.(string)
		route.APIKey = key
	}
	normalized, kwargs := anthBaseClientKwargs(baseURL, secondsDuration(timeout))
	if kwargs.defaultQuery != nil {
		normalized = strings.TrimRight(normalized, "/")
	}
	tr := auxDeadlineTransport(kwargs.connect, kwargs.timeout, kwargs.timeout)
	route.Transport = tr
	built, err := newAnthropicClient(route)
	if err != nil {
		return nil, err
	}
	keyAttr, tokenAttr := anthSDKKeyAttributes(apiKey, baseURL)
	return &anthropicSDKClient{inner: built.(*anthropicClient), baseURL: normalized, transport: tr,
		betas: anthCommonBetasForBaseURL(normalized, dropContext1MBeta), apiKey: keyAttr, authToken: tokenAttr}, nil
}

// Anthropic SDK client for a static key or bearer callable, nil with the error when the build fails
func BuildAnthropicClient(ctx context.Context, apiKey any, baseURL string, timeout float64, dropContext1MBeta bool) (any, error) {
	c, err := buildAnthropicClient(ctx, apiKey, baseURL, timeout, dropContext1MBeta)
	if err != nil || c == nil {
		return nil, err
	}
	return c, nil
}

// Client whose default headers merge the given headers over its own, the SDK with_options call
func (c *anthropicSDKClient) withDefaultHeaders(headers map[string]string) *anthropicSDKClient {
	merged := map[string]string{}
	for k, v := range c.inner.route.Headers {
		merged[k] = v
	}
	for k, v := range headers {
		merged[k] = v
	}
	c.inner.route.Headers = merged
	return c
}

// Request body and per call headers of SDK kwargs, extra_body merged and extra_headers lifted out
func anthSDKRequest(apiKwargs map[string]any, stream bool) (map[string]any, map[string]any) {
	body := make(map[string]any, len(apiKwargs)+1)
	var extraHeaders map[string]any
	for k, v := range apiKwargs {
		switch k {
		case "stream", "extra_body", "extra_query", "timeout":
		case "extra_headers":
			extraHeaders, _ = v.(map[string]any)
		default:
			body[k] = v
		}
	}
	if extra, ok := apiKwargs["extra_body"].(map[string]any); ok {
		anthMergeExtraBody(body, extra)
	}
	if stream {
		body["stream"] = true
	}
	return body, extraHeaders
}

// Messages call aggregated through the stream, falling back to a plain create where streaming is unavailable
func createAnthropicMessage(ctx context.Context, client *anthropicSDKClient, apiKwargs map[string]any, onStreamEvent func(event any) error) (map[string]any, error) {
	return createAnthropicMessageWith(ctx, client, apiKwargs, anthMessageCall{preferStream: true, onStreamEvent: onStreamEvent})
}

// Keyword arguments of one Messages call, the log prefix, stream preference and the two streaming callbacks
type anthMessageCall struct {
	logPrefix     string
	preferStream  bool
	onStreamEvent func(event any) error
	onResponse    func(response any)
}

// Agent Messages call over its anthropic client with the stream preference and the response header hook
func CreateAnthropicMessage(ctx context.Context, client any, apiKwargs map[string]any, logPrefix string, preferStream bool, onResponse func(response any)) (any, error) {
	c, ok := client.(*anthropicSDKClient)
	if !ok {
		return nil, fmt.Errorf("AttributeError: '%T' object has no attribute 'messages'", client)
	}
	return createAnthropicMessageWith(ctx, c, apiKwargs, anthMessageCall{logPrefix: logPrefix, preferStream: preferStream, onResponse: onResponse})
}

// Streams to the final Message when preferred, a plain create when not or when the stream is unavailable
func createAnthropicMessageWith(ctx context.Context, client *anthropicSDKClient, apiKwargs map[string]any, call anthMessageCall) (map[string]any, error) {
	anthSanitizeKwargs(apiKwargs, call.logPrefix)
	if call.preferStream {
		message, err := client.streamFinalMessage(ctx, apiKwargs, call)
		if err == nil {
			return message, nil
		}
		var timeout *AuxTimeoutError
		var cancelled *AuxiliaryExplicitCancellation
		if errors.As(err, &timeout) || errors.As(err, &cancelled) || !client.inner.transport.StreamUnavailable(err) {
			return nil, err
		}
		slog.Debug(fmt.Sprintf("%sAnthropic Messages stream unavailable; falling back to messages.create(): %v", call.logPrefix, err))
	}
	return client.create(ctx, apiKwargs)
}

// Hands the stream's HTTP response to the hook, a failing hook only logged
func anthOfferResponse(call anthMessageCall, resp *http.Response) {
	if call.onResponse == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("%son_response callback failed: %v", call.logPrefix, r))
		}
	}()
	call.onResponse(resp)
}

// Per request httpx limits of a timeout keyword, a number applying to every phase
func anthRequestTimeout(raw any) (pyhttpx.Timeout, bool) {
	if limits, ok := raw.(pyhttpx.Timeout); ok {
		return limits, true
	}
	seconds, ok := finitePositive(raw)
	if !ok {
		return pyhttpx.Timeout{}, false
	}
	d := secondsDuration(seconds)
	return pyhttpx.Timeout{Connect: d, Read: d, Write: d, Pool: d}, true
}

// Posts the Messages request and returns the response with its body still open
func (c *anthropicSDKClient) post(ctx context.Context, apiKwargs map[string]any, stream bool) (*http.Response, error) {
	body, extraHeaders := anthSDKRequest(apiKwargs, stream)
	headers, _, err := c.inner.requestHeaders(ctx, c.betas, extraHeaders)
	if err != nil {
		return nil, err
	}
	httpReq, err := c.inner.transport.Request(ctx, anthOpMessages, body, headers)
	if err != nil {
		return nil, err
	}
	_, connect := anthClientTimeout(0)
	client := c.inner.httpClient(connect)
	if limits, ok := anthRequestTimeout(apiKwargs["timeout"]); ok {
		scoped := *client
		scoped.Transport = pyhttpx.PhaseTimeoutTransport(client.Transport, limits)
		client = &scoped
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		var phased *pyhttpx.TimeoutError
		if errors.As(err, &phased) {
			err = &TimeoutPhaseError{Phase: phased.Phase, Err: err}
		}
		return nil, c.inner.transportError(ctx, err, TimeoutPhaseConnect)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := readLimited(resp.Body, ccErrorBodyLimit)
		return nil, c.inner.httpError(resp, data)
	}
	return resp, nil
}

// Final Message of a streamed call, every event offered to the callback and message_stop required
func (c *anthropicSDKClient) streamFinalMessage(ctx context.Context, apiKwargs map[string]any, call anthMessageCall) (map[string]any, error) {
	resp, err := c.post(ctx, apiKwargs, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	anthOfferResponse(call, resp)
	onStreamEvent := call.onStreamEvent
	events := newAnthSSEEvents(resp.Body, resp.Header)
	acc := newAnthAccumulator()
	var stopDetails any
	sawStop := false
	for {
		ev, err := events.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, c.inner.transportError(ctx, err, TimeoutPhaseRead)
		}
		acc.observe(ev)
		switch ev.Type {
		case "message_stop":
			sawStop = true
		case "message_delta":
			if delta, ok := ev.Payload["delta"].(map[string]any); ok && delta["stop_details"] != nil {
				stopDetails = delta["stop_details"]
			}
		}
		if onStreamEvent == nil {
			continue
		}
		if cbErr := onStreamEvent(ev.Payload); cbErr != nil {
			var timeout *AuxTimeoutError
			var cancelled *AuxiliaryExplicitCancellation
			if errors.As(cbErr, &timeout) || errors.As(cbErr, &cancelled) {
				return nil, cbErr
			}
			slog.Debug(fmt.Sprintf("%son_stream_event callback failed: %v", call.logPrefix, cbErr))
		}
	}
	if !sawStop {
		return nil, &EmptyStreamError{Message: "Anthropic Messages stream ended before message_stop (possible upstream stream drop)."}
	}
	message, err := acc.finalize(true)
	if err != nil {
		return nil, err
	}
	if stopDetails != nil {
		message["stop_details"] = stopDetails
	}
	return message, nil
}

// Raw Message of a non streamed Messages call
func (c *anthropicSDKClient) create(ctx context.Context, apiKwargs map[string]any) (map[string]any, error) {
	resp, err := c.post(ctx, apiKwargs, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, c.inner.transportError(ctx, err, TimeoutPhaseRead)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		apiErr := Classify(c.inner.route.Provider, c.inner.route.Model, resp.StatusCode, pystr.DecodeUTF8Replace(data), err)
		apiErr.Message = "Provider returned a non-JSON message body: " + truncate(redactSensitiveText(strings.TrimSpace(pystr.DecodeUTF8Replace(data))), 300)
		return nil, apiErr
	}
	return payload, nil
}

// AnthropicBedrock client signing with the scoped AWS identity, the 1M context beta and guardrail headers attached
func buildAnthropicBedrockClient(ctx context.Context, region string, timeout float64) (*anthropicSDKClient, error) {
	scoped, err := scopedAWSSessionOptions(ctx)
	if err != nil {
		return nil, err
	}
	bearer, bearerSet := os.LookupEnv("AWS_BEARER_TOKEN_BEDROCK")
	if bearerSet && (scoped.AccessKeyID != "" || scoped.SecretAccessKey != "" || scoped.SessionToken != "" || scoped.Profile != "") {
		return nil, &ValueError{Message: "Cannot specify both `api_key` and AWS credentials (`aws_access_key`, `aws_secret_key`, `aws_session_token`, `aws_profile`)"}
	}
	read, connect := anthClientTimeout(secondsDuration(timeout))
	betas := append(cloneStrings(anthropicCommonBetas), anthropicContext1MBeta)
	headers := bedrockGuardrailHeaders(bedrockProcessConfig(ctx))
	tr := auxDeadlineTransport(connect, read, read)
	route := Route{Provider: "bedrock", Mode: ModeAnthropicMessages, Auth: AuthAWSSigV4, Region: region, Headers: headers, Transport: tr,
		AccessKeyID: scoped.AccessKeyID, SecretAccessKey: scoped.SecretAccessKey, SessionToken: scoped.SessionToken}
	built, err := newAnthropicClient(route)
	if err != nil {
		return nil, err
	}
	return &anthropicSDKClient{inner: built.(*anthropicClient), baseURL: "https://bedrock-runtime." + region + ".amazonaws.com", transport: tr,
		betas: betas, apiKey: bearer}, nil
}

// BuildAnthropicBedrockClient is the Anthropic Bedrock SDK client of a region with the request timeout
func BuildAnthropicBedrockClient(ctx context.Context, region string, timeout float64) (any, error) {
	return buildAnthropicBedrockClient(ctx, region, timeout)
}
