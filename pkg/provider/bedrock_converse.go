package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"
	"weak"

	"github.com/nickheyer/icebath/pkg/state"
)

// Registers the Bedrock Converse adapter
func init() {
	RegisterFactory(modeBedrockConverse, func(route Route) (Client, error) { return newBedrockConverseClient(route) })
}

// Ordered Converse blocks of live assistant messages, gone once the message is collected and never persisted
var bedrockLiveBlocks sync.Map

// Attaches the ordered block sidecar to the in memory message only
func setBedrockLiveBlocks(m *state.Message, blocks []any) {
	key := weak.Make(m)
	bedrockLiveBlocks.Store(key, blocks)
	runtime.AddCleanup(m, func(k weak.Pointer[state.Message]) { bedrockLiveBlocks.Delete(k) }, key)
}

// Ordered block sidecar of a live message, absent for a message reloaded from the session store
func bedrockLiveBlocksOf(m *state.Message) ([]any, bool) {
	v, ok := bedrockLiveBlocks.Load(weak.Make(m))
	if !ok {
		return nil, false
	}
	blocks, ok := v.([]any)
	return blocks, ok && len(blocks) > 0
}

// Stop reasons the raw response accessor maps, the adapter normalizer already applying them
var bedrockTransportStopReasonMap = map[string]string{
	"end_turn": "stop", "tool_use": "tool_calls", "max_tokens": "length", "stop_sequence": "stop",
	"guardrail_intervened": "content_filter", "content_filtered": "content_filter",
}

// Converse client of one route, latching to non streaming once IAM denies ConverseStream
type bedrockConverseClient struct {
	route        Route
	mu           sync.Mutex
	nonStreaming bool
}

func newBedrockConverseClient(route Route) (*bedrockConverseClient, error) {
	return &bedrockConverseClient{route: route}, nil
}

func (c *bedrockConverseClient) Route() Route { return c.route }

// Region of the route, the bedrock-runtime host, then the configured runtime region
func (c *bedrockConverseClient) region(ctx context.Context) string {
	return (&anthBedrockTransport{route: c.route}).region(ctx)
}

// One prepared Converse call
type bedrockConversePrepared struct {
	model  string
	region string
	kwargs map[string]any
}

// Converse request messages as dicts carrying the live ordered block sidecar of each message
func bedrockMessageDicts(messages []*state.Message) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		if m == nil {
			continue
		}
		d := messageToDict(m)
		if blocks, ok := bedrockLiveBlocksOf(m); ok {
			d["bedrock_content_blocks"] = blocks
		}
		out = append(out, d)
	}
	return out
}

// Builds the Converse kwargs of a request the way the Bedrock transport does
func (c *bedrockConverseClient) prepare(ctx context.Context, req *Request) (*bedrockConversePrepared, error) {
	if req == nil {
		return nil, &LocalValidationError{Message: "bedrock converse request is nil"}
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = c.route.Model
	}
	region := c.region(ctx)
	resolve := func(arn string) string { return resolveInferenceProfileModelID(ctx, arn, region) }
	in := bedrockConverseInput{Model: model, Messages: bedrockMessageDicts(req.Messages), Tools: req.Tools,
		Temperature: req.Temperature, GuardrailConfig: BedrockGuardrailConfig(ctx),
		Reasoning: anthReasoningFromRequest(req.Reasoning), Resolve: resolve}
	if req.MaxTokens > 0 {
		maxTokens := req.MaxTokens
		in.MaxTokens = &maxTokens
	}
	return &bedrockConversePrepared{model: model, region: region, kwargs: buildBedrockConverseKwargs(in)}, nil
}

// Sends one Converse or ConverseStream request with the kwargs as the body, the model id in the path
func bedrockConverseSend(ctx context.Context, region string, kwargs map[string]any, stream bool) (*http.Response, error) {
	model := bedrockKwargsModel(kwargs)
	client, err := bedrockRuntimeClient(ctx, region, model)
	if err != nil {
		return nil, err
	}
	body := make(map[string]any, len(kwargs))
	for k, v := range kwargs {
		if k != "modelId" {
			body[k] = v
		}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("provider: encode bedrock converse body: %w", err)
	}
	operation, path, accept := "Converse", "converse", "application/json"
	if stream {
		operation, path, accept = "ConverseStream", "converse-stream", "application/vnd.amazon.eventstream"
	}
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", accept)
	return client.do(ctx, operation, http.MethodPost, bedrockModelPath(model, path), nil, data, headers)
}

// Decoded Converse response body with numbers kept exact
func bedrockDecodeConverse(resp *http.Response) (map[string]any, error) {
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, providerStreamErrorFromNonJSON(string(data), resp.Header)
	}
	return out, nil
}

// Raised when ConverseStream is denied by IAM so the caller can fall back to Converse with the same kwargs
type bedrockStreamDenied struct{ err error }

func (e *bedrockStreamDenied) Error() string { return e.err.Error() }
func (e *bedrockStreamDenied) Unwrap() error { return e.err }

// Opens one call with the shared recovery, a rejected cachePoint or sealed reasoning resent once without it
func bedrockConverseCall(ctx context.Context, region string, kwargs map[string]any, stream, onStreamDenied bool) (*http.Response, error) {
	resp, err := bedrockConverseSend(ctx, region, kwargs, stream)
	if err == nil {
		return resp, nil
	}
	retry, ok := recoverFromBedrockCachePointRejection(err, kwargs)
	if !ok {
		retry, ok = recoverFromBedrockRedactedReasoningRejection(err, kwargs)
	}
	if ok {
		return bedrockConverseSend(ctx, region, retry, stream)
	}
	if onStreamDenied && isBedrockStreamingAccessDenied(err) {
		return nil, &bedrockStreamDenied{err: err}
	}
	if isBedrockStaleConnectionError(err) {
		InvalidateBedrockRuntimeClient(ctx, region)
	}
	return nil, err
}

// Non streaming Converse answer of the kwargs
func bedrockConverseComplete(ctx context.Context, region string, kwargs map[string]any) (*bedrockConverseResult, map[string]any, error) {
	resp, err := bedrockConverseCall(ctx, region, kwargs, false, false)
	if err != nil {
		return nil, nil, err
	}
	raw, err := bedrockDecodeConverse(resp)
	if err != nil {
		return nil, nil, err
	}
	return normalizeConverseResponse(raw), raw, nil
}

// Non streaming Converse call for one shot callers, cachePoint markers skipped when no later call can read them
func callBedrockConverse(ctx context.Context, region string, in bedrockConverseInput) (*bedrockConverseResult, error) {
	in.NoPromptCache = isOneShotRequest(in.Messages, in.Tools)
	if in.Resolve == nil {
		in.Resolve = func(arn string) string { return resolveInferenceProfileModelID(ctx, arn, region) }
	}
	kwargs := buildBedrockConverseKwargs(in)
	resp, err := bedrockConverseSend(ctx, region, kwargs, false)
	if err != nil {
		if retry, ok := recoverFromBedrockCachePointRejection(err, kwargs); ok {
			return bedrockConverseFinish(bedrockConverseSend(ctx, region, retry, false))
		}
		if retry, ok := recoverFromBedrockRedactedReasoningRejection(err, kwargs); ok {
			return bedrockConverseFinish(bedrockConverseSend(ctx, region, retry, false))
		}
		if isBedrockStaleConnectionError(err) {
			bedrockLog.Warn(fmt.Sprintf("bedrock: stale-connection error on converse(region=%s, model=%s): %s — evicting cached client so the next call reconnects.",
				region, in.Model, pyTypeNameOfError(err)))
			InvalidateBedrockRuntimeClient(ctx, region)
		}
		return nil, err
	}
	return bedrockConverseFinish(resp, nil)
}

// Normalized answer of a Converse HTTP response
func bedrockConverseFinish(resp *http.Response, err error) (*bedrockConverseResult, error) {
	if err != nil {
		return nil, err
	}
	raw, err := bedrockDecodeConverse(resp)
	if err != nil {
		return nil, err
	}
	return normalizeConverseResponse(raw), nil
}

// Exception class name a Python caller would log for the error
func pyTypeNameOfError(err error) string {
	var svc *AWSServiceError
	if errors.As(err, &svc) && svc.Code != "" {
		return svc.Code
	}
	var rt *AWSReadTimeoutError
	if errors.As(err, &rt) {
		return "ReadTimeoutError"
	}
	return fmt.Sprintf("%T", err)
}

// Reports a raw Converse body carrying output, the shape the transport accepts
func validateBedrockConverseResponse(raw map[string]any) bool {
	if raw == nil {
		return false
	}
	_, ok := raw["output"]
	return ok
}

// Response of an answer, reasoning details stamped with the answering model and the ordered blocks carried along
func bedrockConverseResponse(r *bedrockConverseResult, model, provider string) *Response {
	msg := &state.Message{Role: "assistant", Content: r.Content, ToolCalls: r.ToolCalls}
	finish := r.FinishReason
	if finish == "" {
		finish = "stop"
	}
	msg.FinishReason = finish
	if r.ReasoningContent != "" {
		msg.Reasoning, msg.ReasoningContent = r.ReasoningContent, r.ReasoningContent
	}
	var details []any
	if len(r.ReasoningDetails) > 0 {
		details = append(details, r.ReasoningDetails...)
		if model != "" {
			details = append(details, map[string]any{"type": BedrockConverseIssuerType, "model": model})
		}
	}
	if len(details) > 0 {
		msg.ReasoningDetails, _ = json.Marshal(details)
	}
	if len(r.ContentBlocks) > 0 {
		setBedrockLiveBlocks(msg, r.ContentBlocks)
	}
	usage := Usage{InputTokens: r.Usage.PromptTokens - r.Usage.CacheReadInputTokens - r.Usage.CacheCreationInputTokens,
		OutputTokens: r.Usage.CompletionTokens, CacheReadTokens: r.Usage.CacheReadInputTokens,
		CacheWriteTokens: r.Usage.CacheCreationInputTokens, TotalTokens: r.Usage.TotalTokens}
	responseModel := r.Model
	if responseModel == "" {
		responseModel = model
	}
	return &Response{Message: msg, FinishReason: finish, Usage: usage, Model: responseModel, Provider: provider}
}

// Complete runs one non streaming Converse call with the shared recovery
func (c *bedrockConverseClient) Complete(ctx context.Context, req *Request) (*Response, error) {
	p, err := c.prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	result, raw, err := bedrockConverseComplete(ctx, p.region, p.kwargs)
	if err != nil {
		return nil, err
	}
	if !validateBedrockConverseResponse(raw) {
		body, _ := json.Marshal(raw)
		return nil, &APIError{Provider: c.route.Provider, Model: p.model, Kind: ErrEmptyResponse, Code: "invalid_response",
			Message: "Bedrock Converse response carried no output", Body: string(body)}
	}
	resp := bedrockConverseResponse(result, p.model, c.route.Provider)
	resp.Raw, _ = json.Marshal(raw)
	resp.Latency = time.Since(started)
	return resp, nil
}

// Stream runs ConverseStream with live deltas, falling back to Converse for the session once IAM denies streaming
func (c *bedrockConverseClient) Stream(ctx context.Context, req *Request, fn StreamFunc) (*Response, error) {
	c.mu.Lock()
	latched := c.nonStreaming
	c.mu.Unlock()
	if latched {
		resp, err := c.Complete(ctx, req)
		if err != nil {
			return nil, err
		}
		return resp, (&anthropicClient{}).replay(resp, fn)
	}
	p, err := c.prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	httpResp, err := bedrockConverseCall(ctx, p.region, p.kwargs, true, true)
	var denied *bedrockStreamDenied
	if errors.As(err, &denied) {
		c.mu.Lock()
		c.nonStreaming = true
		c.mu.Unlock()
		bedrockLog.Info(fmt.Sprintf("bedrock: converse_stream denied by IAM (%s) — using non-streaming converse() for this session.", pyTypeNameOfError(denied.err)))
		result, raw, err := bedrockConverseComplete(ctx, p.region, p.kwargs)
		if err != nil {
			return nil, err
		}
		resp := bedrockConverseResponse(result, p.model, c.route.Provider)
		resp.Raw, _ = json.Marshal(raw)
		resp.Latency = time.Since(started)
		return resp, (&anthropicClient{}).replay(resp, fn)
	}
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	emit := func(ev StreamEvent) error {
		if fn == nil {
			return nil
		}
		return fn(ev)
	}
	cb := bedrockStreamCallbacks{
		OnTextDelta:      func(text string) error { return emit(StreamEvent{Kind: EventTextDelta, Text: text}) },
		OnReasoningDelta: func(text string) error { return emit(StreamEvent{Kind: EventReasoningDelta, Text: text}) },
		OnToolStart: func(index int, id, name string) error {
			return emit(StreamEvent{Kind: EventToolCallStart, Index: index, ToolCall: &state.ToolCall{ID: id, Type: "function", Function: state.ToolCallFunction{Name: name}}})
		},
		OnToolDone: func(index int, call state.ToolCall) error {
			return emit(StreamEvent{Kind: EventToolCallDone, Index: index, ToolCall: &call})
		},
		OnInterruptCheck: func() bool { return ctx.Err() != nil },
	}
	result, interrupted, err := streamConverseWithCallbacks(bedrockConverseEvents(httpResp.Body, httpResp.Header.Clone()), cb)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	resp := bedrockConverseResponse(result, p.model, c.route.Provider)
	resp.Headers = httpResp.Header.Clone()
	resp.StatusCode = httpResp.StatusCode
	resp.RequestID = httpResp.Header.Get("X-Amzn-Requestid")
	resp.Latency = time.Since(started)
	if interrupted {
		return resp, ctx.Err()
	}
	usage := resp.Usage
	if err := emit(StreamEvent{Kind: EventUsage, Usage: &usage}); err != nil {
		return resp, err
	}
	return resp, emit(StreamEvent{Kind: EventDone})
}

// CountTokens estimates the Converse body the request would send
func (c *bedrockConverseClient) CountTokens(ctx context.Context, req *Request) (int, error) {
	p, err := c.prepare(ctx, req)
	if err != nil {
		return 0, err
	}
	return estimateBodyContextTokens(p.kwargs, DefaultImageTokenCost), nil
}

// Raw Converse body or a normalized answer as the transport's normalized response, stamped with the model
func bedrockTransportNormalize(response any, model string) (*Response, error) {
	var result *bedrockConverseResult
	switch r := response.(type) {
	case *bedrockConverseResult:
		result = r
	case map[string]any:
		result = normalizeConverseResponse(r)
	default:
		return nil, fmt.Errorf("provider: bedrock converse response of type %T", response)
	}
	return bedrockConverseResponse(result, model, "bedrock"), nil
}

// Raw stop reason mapped for raw response access, stop for anything unknown
func bedrockTransportFinishReason(stopReason string) string {
	if v, ok := bedrockTransportStopReasonMap[stopReason]; ok {
		return v
	}
	return "stop"
}
