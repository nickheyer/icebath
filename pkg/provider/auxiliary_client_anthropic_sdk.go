package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Anthropic SDK style client over the Messages wire, taking raw api kwargs and returning raw Message maps
type anthropicSDKClient struct {
	inner     *anthropicClient
	baseURL   string
	betas     []string
	transport *http.Transport
	closed    atomic.Bool
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

// Trailing v1 segment the SDK appends itself
var anthSDKV1Suffix = regexp.MustCompile(`/v1/?$`)

// Bearer token source of a callable key, false for a static key
func anthSDKTokenSource(apiKey any) (func(context.Context) (string, error), bool) {
	switch k := apiKey.(type) {
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
	read := 900 * time.Second
	if timeout > 0 {
		read = time.Duration(timeout * float64(time.Second))
	}
	route := Route{Provider: "anthropic", Mode: ModeAnthropicMessages, BaseURL: baseURL}
	if src, ok := anthSDKTokenSource(apiKey); ok {
		route.TokenSource = src
	} else {
		oauthNormalizeProxyEnvVars()
		key, _ := apiKey.(string)
		route.APIKey = key
	}
	tr := auxDeadlineTransport(10*time.Second, read, read)
	route.Transport = tr
	built, err := newAnthropicClient(route)
	if err != nil {
		return nil, err
	}
	normalized := anthSDKV1Suffix.ReplaceAllString(strings.TrimRight(anthNormalizedBaseURL(baseURL), "/"), "")
	return &anthropicSDKClient{inner: built.(*anthropicClient), baseURL: normalized, transport: tr,
		betas: anthCommonBetasForBaseURL(normalized, dropContext1MBeta)}, nil
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
	anthSanitizeKwargs(apiKwargs, "")
	message, err := client.streamFinalMessage(ctx, apiKwargs, onStreamEvent)
	if err == nil {
		return message, nil
	}
	var timeout *AuxTimeoutError
	var cancelled *AuxiliaryExplicitCancellation
	if errors.As(err, &timeout) || errors.As(err, &cancelled) || !client.inner.transport.StreamUnavailable(err) {
		return nil, err
	}
	slog.Debug(fmt.Sprintf("Anthropic Messages stream unavailable; falling back to messages.create(): %v", err))
	return client.create(ctx, apiKwargs)
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
	resp, err := c.inner.httpClient(10 * time.Second).Do(httpReq)
	if err != nil {
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
func (c *anthropicSDKClient) streamFinalMessage(ctx context.Context, apiKwargs map[string]any, onStreamEvent func(event any) error) (map[string]any, error) {
	resp, err := c.post(ctx, apiKwargs, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
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
			slog.Debug(fmt.Sprintf("on_stream_event callback failed: %v", cbErr))
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
