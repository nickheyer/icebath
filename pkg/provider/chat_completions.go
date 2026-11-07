package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Registers the OpenAI chat completions and Codex Responses adapters
func init() {
	RegisterFactory(ModeChatCompletions, func(route Route) (Client, error) { return newChatCompletionsClient(route) })
	RegisterFactory(ModeCodexResponses, func(route Route) (Client, error) { return newCodexResponsesClient(route) })
}

// Header names carrying the provider request id, in lookup order
var requestIDHeaders = []string{"x-request-id", "request-id", "x-amzn-requestid", "cf-ray"}

// Client speaking the OpenAI chat completions wire for one route
type chatCompletionsClient struct {
	route                 Route
	cc                    *ccRoute
	llamacpp              bool
	mu                    sync.Mutex
	streamOptionsRejected bool
	nonStreaming          bool
	lastAliases           map[string]string
	transportsMu          sync.Mutex
	transports            map[time.Duration]*http.Transport
}

func newChatCompletionsClient(route Route) (Client, error) {
	if strings.TrimSpace(route.BaseURL) == "" {
		return nil, fmt.Errorf("provider: chat completions route for %s/%s has no base URL", route.Provider, route.Model)
	}
	return &chatCompletionsClient{route: route, cc: newCCRoute(route), llamacpp: streamsLlamaCpp(route), transports: map[time.Duration]*http.Transport{}}, nil
}

func (c *chatCompletionsClient) Route() Route { return c.route }

// HTTP client for the route, dial and handshake bounded by the connect timeout
func (c *chatCompletionsClient) httpClient(connect time.Duration) *http.Client {
	if c.route.Transport != nil {
		return &http.Client{Transport: c.route.Transport}
	}
	c.transportsMu.Lock()
	defer c.transportsMu.Unlock()
	tr, ok := c.transports[connect]
	if !ok {
		tr = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: connect, KeepAlive: 30 * time.Second}).DialContext,
			TLSClientConfig:       PlatformTLSConfig(),
			TLSHandshakeTimeout:   connect,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			ExpectContinueTimeout: time.Second,
		}
		c.transports[connect] = auxTrackPoolConns(tr)
	}
	return &http.Client{Transport: tr}
}

// One request shaped for the wire with its URL, headers and context estimate
type ccPrepared struct {
	build     *ccBuild
	body      []byte
	wire      map[string]any
	url       string
	headers   map[string]string
	settings  wireSettings
	estTokens int
}

// Reports whether stream_options include_usage goes on the wire for this request
func (c *chatCompletionsClient) includeUsage(req *Request, rejected bool) bool {
	if rejected || c.cc.isNativeGemini() {
		return false
	}
	return req == nil || req.Metadata[StreamOptionsOffKey] != "off"
}

// Builds the wire body, endpoint URL and headers for one request
func (c *chatCompletionsClient) prepare(ctx context.Context, req *Request, stream bool) (*ccPrepared, error) {
	settings := loadWireSettings(ctx)
	build := c.cc.build(ctx, req, settings)
	if build.err != nil {
		return nil, build.err
	}
	c.mu.Lock()
	c.lastAliases = build.aliases
	rejected := c.streamOptionsRejected
	c.mu.Unlock()
	wire := newOmap()
	for _, k := range build.body.keys {
		wire.Set(k, build.body.vals[k])
	}
	for _, k := range build.extraBody.keys {
		wire.Set(k, build.extraBody.vals[k])
	}
	if stream {
		wire.Set("stream", true)
		if c.includeUsage(req, rejected) {
			wire.Set("stream_options", map[string]any{"include_usage": true})
		} else {
			wire.Del("stream_options")
		}
		if c.llamacpp && !wire.Has("return_progress") {
			wire.Set("return_progress", true)
		}
	} else {
		wire.Del("stream")
		wire.Del("stream_options")
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("provider: encode chat completions body: %w", err)
	}
	endpoint, err := ccEndpointURL(c.route, "/chat/completions")
	if err != nil {
		return nil, err
	}
	headers := make(map[string]string, len(build.headers)+2)
	for k, v := range build.headers {
		headers[k] = v
	}
	auth, strip, err := ccAuthHeaders(ctx, c.route)
	if err != nil {
		return nil, Classify(c.route.Provider, c.route.Model, 401, "", err)
	}
	for _, name := range strip {
		for k := range headers {
			if strings.EqualFold(k, name) {
				delete(headers, k)
			}
		}
	}
	for k, v := range auth {
		headers[k] = v
	}
	wireMap := wire.Map()
	return &ccPrepared{build: build, body: body, wire: wireMap, url: endpoint, headers: headers, settings: settings,
		estTokens: EstimateRequestContextTokens(ctx, wireMap)}, nil
}

func (c *chatCompletionsClient) newRequest(ctx context.Context, p *ccPrepared) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(p.body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range p.headers {
		httpReq.Header.Set(k, v)
	}
	applyProviderRequestHeaders(ctx, httpReq.Header)
	return httpReq, nil
}

// Error of a non success HTTP response carrying the body and headers for classification
type ccHTTPError struct {
	Status  int
	Body    string
	Headers http.Header
}

func (e *ccHTTPError) Error() string {
	msg := strings.TrimSpace(e.Body)
	if m := decodeJSONObject(e.Body); m != nil {
		if s := stringField(errorObj(m), "message"); s != "" {
			msg = s
		}
	}
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, truncate(msg, 500))
}

// ResponseHeaders exposes the headers for reset and welcome tier parsing
func (e *ccHTTPError) ResponseHeaders() http.Header { return e.Headers }

func headerRequestID(h http.Header) string {
	for _, name := range requestIDHeaders {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			return v
		}
	}
	return ""
}

// APIError for a non success HTTP response
func (c *chatCompletionsClient) httpError(resp *http.Response, data []byte) error {
	body := pystr.DecodeUTF8Replace(data)
	he := &ccHTTPError{Status: resp.StatusCode, Body: body, Headers: resp.Header.Clone()}
	apiErr := Classify(c.route.Provider, c.route.Model, resp.StatusCode, body, he)
	if apiErr.RetryAfter == 0 {
		apiErr.RetryAfter = RetryAfterFrom(resp.Header.Get("Retry-After"))
	}
	if apiErr.RequestID == "" {
		apiErr.RequestID = headerRequestID(resp.Header)
	}
	if apiErr.Err == nil {
		apiErr.Err = he
	}
	return apiErr
}

// APIError for a transport failure before any response arrived
func (c *chatCompletionsClient) transportError(ctx context.Context, err error, phase string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	wrapped := ccWrapTransport(err, phase)
	return Classify(c.route.Provider, c.route.Model, 0, "", wrapped)
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit))
}

// Complete issues one blocking chat completion
func (c *chatCompletionsClient) Complete(ctx context.Context, req *Request) (*Response, error) {
	p, err := c.prepare(ctx, req, false)
	if err != nil {
		return nil, err
	}
	stale := computeNonStreamStaleTimeout(ctx, p.settings, c.cc.provider, req, c.cc.local, false, p.estTokens, map[string]any{"extra_body": geminiPlain(p.build.extraBody)})
	return runNonStreamRequest(ctx, req.Model, p.estTokens, stale, func(ctx context.Context) (*Response, error) { return c.completeOnce(ctx, req, p) })
}

// One blocking chat completion on the request worker
func (c *chatCompletionsClient) completeOnce(ctx context.Context, req *Request, p *ccPrepared) (*Response, error) {
	base, configured := c.requestTimeout(req, p.settings)
	connect := 30 * time.Second
	if configured {
		connect = base
		if connect > 60*time.Second {
			connect = 60 * time.Second
		}
	}
	start := time.Now()
	rctx, cancel := context.WithTimeout(ctx, base)
	defer cancel()
	httpReq, err := c.newRequest(rctx, p)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient(connect).Do(httpReq)
	if err != nil {
		if ctx.Err() == nil && errors.Is(rctx.Err(), context.DeadlineExceeded) {
			err = &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: fmt.Errorf("Request timed out after %.0fs.", base.Seconds())}
		}
		return nil, c.transportError(ctx, err, TimeoutPhaseConnect)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		if ctx.Err() == nil && errors.Is(rctx.Err(), context.DeadlineExceeded) {
			err = &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: fmt.Errorf("Request timed out after %.0fs.", base.Seconds())}
		}
		return nil, c.transportError(ctx, err, TimeoutPhaseRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.httpError(resp, data)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		apiErr := Classify(c.route.Provider, c.route.Model, resp.StatusCode, pystr.DecodeUTF8Replace(data), err)
		apiErr.Message = "Provider returned a non-JSON completion body: " + truncate(redactSensitiveText(strings.TrimSpace(pystr.DecodeUTF8Replace(data))), 300)
		apiErr.Err = &ccHeadersError{err: err, headers: resp.Header.Clone()}
		return nil, apiErr
	}
	return c.normalizeResponse(payload, resp.StatusCode, resp.Header.Clone(), time.Since(start))
}

// Stream issues one streamed chat completion, renegotiating stream_options once when rejected
func (c *chatCompletionsClient) Stream(ctx context.Context, req *Request, fn StreamFunc) (*Response, error) {
	c.mu.Lock()
	latched := c.nonStreaming
	c.mu.Unlock()
	if latched {
		resp, err := c.Complete(ctx, req)
		if err != nil {
			return nil, err
		}
		return resp, c.replay(resp, fn)
	}
	for attempt := 0; attempt < 2; attempt++ {
		p, err := c.prepare(ctx, req, true)
		if err != nil {
			return nil, err
		}
		resp, deltasSent, err := c.streamOnce(ctx, req, p, fn)
		if err == nil {
			return resp, nil
		}
		var apiErr *APIError
		if attempt == 0 && !deltasSent && errors.As(err, &apiErr) && rejectsStreamOptions(apiErr) {
			c.mu.Lock()
			already := c.streamOptionsRejected
			c.streamOptionsRejected = true
			c.mu.Unlock()
			if !already {
				catalogLog.Info(fmt.Sprintf("Endpoint rejected stream_options (HTTP %d); retrying without it for this client.", apiErr.StatusCode))
				continue
			}
		}
		return resp, err
	}
	return nil, errors.New("provider: stream attempt loop ended without a response")
}

// Fresh per attempt stream state with its watches, diagnostics and failed request record
func (c *chatCompletionsClient) newStreamState(ctx context.Context, req *Request, p *ccPrepared, fn StreamFunc, t ccTimeouts) *ccStreamState {
	s := &ccStreamState{c: c, req: req, fn: fn, estTokens: p.estTokens, tools: newCCToolCallAccumulator(), scrubber: NewStreamingThinkScrubber(),
		contentWatch: NewRunawayStreamWatch(), reasoningWatch: NewRunawayStreamWatch(), detailWatch: NewRunawayStreamWatch(),
		started: time.Now(), lifetimeCap: t.lifetime.Seconds()}
	s.diag = streamCallFrom(ctx).newDiag()
	s.wire = &FailedStreamRequest{URL: p.url, Content: p.body}
	return s
}

// Runs one streaming attempt and reports whether visible deltas reached the consumer
func (c *chatCompletionsClient) streamOnce(ctx context.Context, req *Request, p *ccPrepared, fn StreamFunc) (*Response, bool, error) {
	if _, managed := relayCurrentSessionID(ctx); managed {
		return chatManagedRelayStream(ctx, c, req, p, fn)
	}
	t := c.streamTimeouts(req, p.settings, p.estTokens)
	s := c.newStreamState(ctx, req, p, fn, t)
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	kill := &ccKill{}
	httpReq, err := c.newRequest(attemptCtx, p)
	if err != nil {
		return nil, false, err
	}
	stopHdrStale := armHeaderStaleKill(ctx, cancel, kill, s.emit, s.req.Model, s.estTokens, t.stale, s.started)
	hdrTimer := time.AfterFunc(t.read, func() {
		kill.fire("read", t.read.Seconds())
		cancel()
	})
	resp, err := c.httpClient(t.connect).Do(httpReq)
	hdrTimer.Stop()
	stopHdrStale()
	if err != nil {
		return nil, false, c.attemptError(attemptCtx, kill, err, s)
	}
	defer resp.Body.Close()
	if reabortIfCancelled(attemptCtx, resp) {
		return nil, false, c.attemptError(attemptCtx, kill, context.Cause(attemptCtx), s)
	}
	s.headers = resp.Header.Clone()
	s.status = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := readLimited(resp.Body, ccErrorBodyLimit)
		return nil, false, c.httpError(resp, data)
	}
	StreamDiagCaptureResponse(s.diag, resp)
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/json") && !strings.Contains(ct, "event-stream") {
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, false, c.attemptError(attemptCtx, kill, err, s)
		}
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			return nil, false, c.streamError(providerStreamErrorFromNonJSON(string(data), s.headers))
		}
		if eo, ok := payload["error"]; ok && eo != nil {
			if em, ok := eo.(map[string]any); !ok || len(em) > 0 {
				return nil, false, c.httpError(resp, data)
			}
		}
		catalogLog.Info(fmt.Sprintf("Streaming request returned a final response object instead of an iterator; switching %s/%s to non-streaming for this session.", orUnknown2(c.route.Provider), orUnknown2(c.route.Model)))
		c.mu.Lock()
		c.nonStreaming = true
		c.mu.Unlock()
		final, err := c.normalizeResponse(payload, resp.StatusCode, s.headers, time.Since(s.started))
		if err != nil {
			return nil, false, err
		}
		return final, false, c.replay(final, fn)
	}
	s.claimWriter(ctx)
	out, err := c.consumeStream(ctx, attemptCtx, cancel, kill, newSSEReader(resp.Body), nil, s, t)
	return out, s.deltasSent, err
}

// Replays a completed response's reasoning, text, tool calls and usage as stream events
func (c *chatCompletionsClient) replay(resp *Response, fn StreamFunc) error {
	if fn == nil || resp == nil || resp.Message == nil {
		return nil
	}
	if resp.Message.Reasoning != "" {
		if err := fn(StreamEvent{Kind: EventReasoningDelta, Text: resp.Message.Reasoning}); err != nil {
			return err
		}
	}
	if text := resp.Message.Text(); text != "" {
		scrubber := NewStreamingThinkScrubber()
		visible := strings.TrimLeft(scrubber.Feed(text)+scrubber.Flush(), "\n")
		if visible != "" {
			if err := fn(StreamEvent{Kind: EventTextDelta, Text: visible}); err != nil {
				return err
			}
		}
	}
	for i := range resp.Message.ToolCalls {
		tc := resp.Message.ToolCalls[i]
		if err := fn(StreamEvent{Kind: EventToolCallDone, Index: i, ToolCall: &tc}); err != nil {
			return err
		}
	}
	usage := resp.Usage
	if err := fn(StreamEvent{Kind: EventUsage, Usage: &usage}); err != nil {
		return err
	}
	return fn(StreamEvent{Kind: EventDone})
}

// CountTokens estimates the context the request occupies on this wire
func (c *chatCompletionsClient) CountTokens(ctx context.Context, req *Request) (int, error) {
	p, err := c.prepare(ctx, req, req != nil && req.Stream)
	if err != nil {
		return 0, err
	}
	return p.estTokens, nil
}

// ---- response normalization ----

// Usage of a chat completion payload with cache and reasoning detail buckets
func ccUsageFromMap(u map[string]any) Usage {
	out := Usage{}
	if u == nil {
		return out
	}
	out.InputTokens, _ = anyInt(u["prompt_tokens"])
	out.OutputTokens, _ = anyInt(u["completion_tokens"])
	out.TotalTokens, _ = anyInt(u["total_tokens"])
	if details, ok := u["prompt_tokens_details"].(map[string]any); ok {
		out.CacheReadTokens, _ = anyInt(details["cached_tokens"])
		out.CacheWriteTokens, _ = anyInt(details["cache_write_tokens"])
	}
	if out.CacheReadTokens == 0 {
		out.CacheReadTokens, _ = anyInt(u["prompt_cache_hit_tokens"])
	}
	if details, ok := u["completion_tokens_details"].(map[string]any); ok {
		out.ReasoningTokens, _ = anyInt(details["reasoning_tokens"])
	}
	if out.TotalTokens <= 0 {
		out.TotalTokens = out.InputTokens + out.OutputTokens
	}
	return out
}

var inlineReasoningRes = func() []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, tag := range ThinkTagNames {
		out = append(out, regexp.MustCompile(`(?is)<`+regexp.QuoteMeta(tag)+`>(.*?)</`+regexp.QuoteMeta(tag)+`>`))
	}
	return out
}()

// Reasoning text from the structured fields, else thinking blocks or inline tags in the content
func ccExtractReasoning(plain, reasoningContent string, details []any, content any) string {
	var parts []string
	add := func(v any) {
		text := flattenMessageText(v, "")
		if text == "" {
			return
		}
		for _, p := range parts {
			if p == text {
				return
			}
		}
		parts = append(parts, text)
	}
	add(plain)
	add(reasoningContent)
	for _, d := range details {
		dm, ok := d.(map[string]any)
		if !ok {
			continue
		}
		for _, k := range []string{"summary", "thinking", "content", "text"} {
			if !detailValueEmpty(dm[k]) {
				add(dm[k])
				break
			}
		}
	}
	if len(parts) == 0 {
		if list, ok := content.([]any); ok {
			for _, b := range list {
				bm, ok := b.(map[string]any)
				if !ok || stringOf(bm["type"]) != "thinking" {
					continue
				}
				v := bm["thinking"]
				if detailValueEmpty(v) {
					v = bm["text"]
				}
				add(strings.TrimSpace(flattenMessageText(v, "")))
			}
		}
	}
	if len(parts) == 0 {
		if text, ok := content.(string); ok && text != "" {
			for _, re := range inlineReasoningRes {
				for _, m := range re.FindAllStringSubmatch(text, -1) {
					add(strings.TrimSpace(m[1]))
				}
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// Private reasoning_details record carrying provider native assistant carriers for replay
func ccNativeCarrierRecord(signature string, toolCarriers []map[string]any) map[string]any {
	if signature == "" && len(toolCarriers) == 0 {
		return nil
	}
	rec := map[string]any{"type": nativeCarrierType}
	if signature != "" {
		rec["extra_content"] = map[string]any{"google": map[string]any{"thought_signature": signature}}
	}
	if len(toolCarriers) > 0 {
		list := make([]any, 0, len(toolCarriers))
		for _, tc := range toolCarriers {
			list = append(list, tc)
		}
		rec["tool_calls"] = list
	}
	return rec
}

// Normalizes a chat completion payload into the provider neutral response
func (c *chatCompletionsClient) normalizeResponse(payload map[string]any, status int, headers http.Header, latency time.Duration) (*Response, error) {
	raw, _ := json.Marshal(payload)
	choices, _ := payload["choices"].([]any)
	if len(choices) == 0 {
		return nil, &APIError{Provider: c.route.Provider, Model: c.route.Model, StatusCode: status, Kind: ErrEmptyResponse, Code: "no_choices",
			Message: "Provider returned a completion with no choices", Body: string(raw), RequestID: headerRequestID(headers), Err: &ccHeadersError{err: errors.New("completion carried no choices"), headers: headers}}
	}
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	if msg == nil {
		msg = map[string]any{}
	}
	finish, _ := NormalizeFinishReason(ccFinishReasonText(choice["finish_reason"])).(string)
	if finish == "" {
		finish = "stop"
	}
	c.mu.Lock()
	aliases := c.lastAliases
	c.mu.Unlock()
	var toolCalls []state.ToolCall
	var toolCarriers []map[string]any
	if list, ok := msg["tool_calls"].([]any); ok {
		for i, rawTC := range list {
			tc, _ := rawTC.(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			if fn == nil || fn["name"] == nil {
				continue
			}
			name := stringOf(fn["name"])
			if aliases != nil {
				if orig, ok := aliases[name]; ok {
					name = orig
				}
			} else if name == xaiToolSearchAlias {
				name = "tool_search"
			}
			args := "{}"
			switch a := fn["arguments"].(type) {
			case nil:
			case string:
				args = a
			default:
				b, _ := json.Marshal(a)
				args = string(b)
			}
			id := strings.TrimSpace(ccDeltaID(tc["id"]))
			if id == "" {
				id = tools.DeterministicCallID(name, args, i)
			}
			typ := stringOf(tc["type"])
			if typ == "" {
				typ = "function"
			}
			toolCalls = append(toolCalls, state.ToolCall{ID: id, Type: typ, Function: state.ToolCallFunction{Name: name, Arguments: args}})
			carrier := map[string]any{}
			if extra := tc["extra_content"]; extra != nil {
				carrier["extra_content"] = extra
			}
			if sig, ok := fn["thought_signature"].(string); ok && sig != "" {
				carrier["thought_signature"] = sig
			}
			if len(carrier) > 0 {
				carrier["id"] = id
				toolCarriers = append(toolCarriers, carrier)
			}
		}
	}
	var usage Usage
	if u, ok := payload["usage"].(map[string]any); ok {
		usage = ccUsageFromMap(u)
	}
	reasoningContent, _ := msg["reasoning_content"].(string)
	reasoningPlain, _ := msg["reasoning"].(string)
	details, _ := msg["reasoning_details"].([]any)
	signature := thoughtSignature(msg["extra_content"])
	content := msg["content"]
	if refusal, ok := msg["refusal"].(string); ok && strings.TrimSpace(refusal) != "" {
		textContent, isText := content.(string)
		if (content == nil || (isText && strings.TrimSpace(textContent) == "")) && len(toolCalls) == 0 {
			content = refusal
			if finish == "stop" {
				finish = "content_filter"
			}
		}
	}
	if list, ok := content.([]map[string]any); ok {
		conv := make([]any, 0, len(list))
		for _, p := range list {
			conv = append(conv, p)
		}
		content = conv
	}
	reasoning := ccExtractReasoning(reasoningPlain, reasoningContent, details, content)
	message := &state.Message{Role: "assistant", Content: content, ToolCalls: toolCalls, Reasoning: reasoning, FinishReason: finish}
	if reasoningContent != "" {
		message.ReasoningContent = reasoningContent
	} else if reasoning != "" {
		message.ReasoningContent = reasoning
	}
	kept := make([]any, 0, len(details)+1)
	for _, d := range details {
		if dm, ok := d.(map[string]any); ok {
			kept = append(kept, dm)
		}
	}
	if rec := ccNativeCarrierRecord(signature, toolCarriers); rec != nil {
		kept = append(kept, rec)
	}
	if len(kept) > 0 {
		message.ReasoningDetails, _ = json.Marshal(kept)
	}
	text, _ := content.(string)
	if isRouterTimeoutShim(text, len(toolCalls), usage) {
		return nil, &APIError{Provider: c.route.Provider, Model: c.route.Model, StatusCode: status, Kind: ErrEmptyResponse, Code: "router_timeout_shim",
			Message: "Provider returned a router timeout shim instead of a completion: " + routerTimeoutShim, Body: string(raw), RequestID: headerRequestID(headers),
			Err: &ccHeadersError{err: errors.New(routerTimeoutShim), headers: headers}}
	}
	model := stringOf(payload["model"])
	if model == "" {
		model = c.route.Model
	}
	requestID := stringOf(payload["id"])
	if requestID == "" {
		requestID = headerRequestID(headers)
	}
	return &Response{Message: message, FinishReason: finish, Usage: usage, Model: model, Provider: c.route.Provider, RequestID: requestID,
		StatusCode: status, Headers: headers, Raw: raw, Latency: latency, Truncated: finish == "length"}, nil
}
