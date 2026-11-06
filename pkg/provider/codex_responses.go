package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Request metadata keys the agent sets to steer a Codex Responses call
const (
	// Set to off after a repeat invalid_encrypted_content 400 to stop replaying encrypted reasoning
	CodexReasoningReplayOffKey = "codex_reasoning_replay"
	// Set to off after a context_management rejection to stop native compaction for the session
	CodexNativeCompactionOffKey = "codex_native_compaction"
	// Truthy when a trusted proxy implements OpenAI native compaction
	CodexTrustedProxyCompactionKey = "openai_native_compaction"
	// Local compressor trigger in tokens that bounds the native compact threshold
	CompressionTriggerTokensKey = "compression_trigger_tokens"
)

// CodexReplayControl exposes the per client replay recoveries the agent runs on a Codex 400
type CodexReplayControl interface {
	RejectAllTurns(err error) bool
	DropUnverifiedReplay(messageLists ...[]*state.Message) int
}

// Client for the OpenAI Responses wire with the Codex stream watchdogs
type codexResponsesClient struct {
	route        Route
	cc           *ccRoute
	flags        responsesRouteFlags
	transport    *codexTransport
	transportsMu sync.Mutex
	transports   map[time.Duration]*http.Transport
}

func newCodexResponsesClient(route Route) (Client, error) {
	if strings.TrimSpace(route.BaseURL) == "" {
		return nil, errors.New("provider: codex responses route has no base URL")
	}
	return &codexResponsesClient{route: route, cc: newCCRoute(route), flags: classifyResponsesRoute(route.Provider, route.BaseURL),
		transport: newCodexTransport(), transports: map[time.Duration]*http.Transport{}}, nil
}

func (c *codexResponsesClient) Route() Route { return c.route }

// RejectAllTurns omits reasoning.context for the model after a 400 naming it
func (c *codexResponsesClient) RejectAllTurns(err error) bool { return c.transport.RejectAllTurns(err) }

// DropUnverifiedReplay strips replayed blobs the last request could not vouch for
func (c *codexResponsesClient) DropUnverifiedReplay(messageLists ...[]*state.Message) int {
	return c.transport.DropUnverifiedReplay(messageLists...)
}

// HTTP client with a cached transport per connect timeout
func (c *codexResponsesClient) httpClient(connect time.Duration) *http.Client {
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
		c.transports[connect] = tr
	}
	return &http.Client{Transport: tr}
}

// Overall request timeout and whether configuration supplied it
func (c *codexResponsesClient) requestTimeout(req *Request, settings wireSettings) (time.Duration, bool) {
	if req != nil && req.Timeout > 0 {
		return req.Timeout, true
	}
	if c.route.Timeout > 0 {
		return c.route.Timeout, true
	}
	if d, ok := configuredProviderTimeout(settings.providers, c.cc.provider, req.Model, "timeout_seconds", "request_timeout_seconds"); ok {
		return d, true
	}
	return 0, false
}

// Whether compression.checkpoint_required is explicitly enabled in the home configuration
func codexCheckpointRequired(ctx context.Context) bool {
	h := home.Dir(ctx)
	tree, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		tree = config.Defaults()
	}
	v, ok := tree.Get("compression.checkpoint_required").(bool)
	return ok && v
}

// Request overrides merged from the route, the request body extras and the request fields
func (c *codexResponsesClient) requestOverrides(req *Request) map[string]any {
	overrides := map[string]any{}
	for k, v := range c.route.ExtraBody {
		overrides[k] = v
	}
	for k, v := range req.ExtraBody {
		overrides[k] = v
	}
	if req.ServiceTier != "" {
		if _, has := overrides["service_tier"]; !has {
			overrides["service_tier"] = req.ServiceTier
		}
	}
	if req.ToolChoice != nil {
		if _, has := overrides["tool_choice"]; !has {
			overrides["tool_choice"] = req.ToolChoice
		}
	}
	if req.ParallelToolCalls != nil {
		if _, has := overrides["parallel_tool_calls"]; !has {
			overrides["parallel_tool_calls"] = *req.ParallelToolCalls
		}
	}
	if len(req.ResponseFormat) > 0 {
		if _, has := overrides["text"]; !has {
			overrides["text"] = codexTextFormat(req.ResponseFormat)
		}
	}
	return overrides
}

// Truthy request metadata flag, the strings 1, true, yes and on
func metadataTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Responses text.format object from a chat completions response_format
func codexTextFormat(rf map[string]any) map[string]any {
	format := map[string]any{}
	kind := stringOf(rf["type"])
	if kind == "" {
		kind = "text"
	}
	format["type"] = kind
	if schema, ok := rf["json_schema"].(map[string]any); ok {
		for k, v := range schema {
			format[k] = v
		}
	}
	return map[string]any{"format": format}
}

// One request shaped for the wire
type codexPrepared struct {
	kwargs    map[string]any
	body      []byte
	wire      map[string]any
	url       string
	headers   map[string]string
	settings  wireSettings
	estTokens int
	timeout   time.Duration
	watch     codexWatchdogs
}

// Native compaction context management this route sends for the request, empty when ineligible
func (c *codexResponsesClient) nativeContextManagement(ctx context.Context, req *Request, model string, settings wireSettings, endpoint *responsesEndpointQuirks) []map[string]any {
	runtimeCapability := resolveNativeCompactionCapability(model, c.route.BaseURL, c.route.Provider, c.flags.isCodexBackend)
	if req.Metadata[CodexNativeCompactionOffKey] == "off" {
		runtimeCapability = false
	}
	var localTrigger any
	if raw := strings.TrimSpace(req.Metadata[CompressionTriggerTokensKey]); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			localTrigger = n
		}
	}
	return nativeCompactionContextManagement(nativeCompactionInputs{
		runtimeCapability: runtimeCapability, configured: settings.nativeCompaction, compressionEnabled: settings.compressionEnabled,
		checkpointRequired: codexCheckpointRequired(ctx), model: model, provider: c.route.Provider, baseURL: c.route.BaseURL,
		trustedProxy:       pyTruthy(req.Metadata[CodexTrustedProxyCompactionKey]) && strings.ToLower(req.Metadata[CodexTrustedProxyCompactionKey]) != "false" && req.Metadata[CodexTrustedProxyCompactionKey] != "0",
		localTriggerTokens: localTrigger, compactThreshold: settings.compactThreshold, isCodexBackend: c.flags.isCodexBackend,
		isXAI: c.flags.isXAI, endpoint: endpoint,
	})
}

// EstimateNativePreflightTokens estimates the checkpoint pruned Responses input, false when native compaction is ineligible or conversion fails
func (c *codexResponsesClient) EstimateNativePreflightTokens(ctx context.Context, req *Request, systemPrompt string, tools []map[string]any) (int, bool) {
	if req == nil || req.Messages == nil {
		return 0, false
	}
	model := req.Model
	if model == "" {
		model = c.route.Model
	}
	endpoint := c.flags.endpoint
	if !nativeCompactionActive(c.nativeContextManagement(ctx, req, model, loadWireSettings(ctx), &endpoint)) {
		return 0, false
	}
	if override, ok := c.requestOverrides(req)["model"].(string); ok && override != "" {
		model = override
	}
	items, err := chatMessagesToResponsesInput(codexMsgsOf(req.Messages), codexConvertOptions{
		isXAI: c.flags.isXAI, dropReplayItemIDs: !endpoint.replayItemIDs, replayEncryptedReasoning: req.Metadata[CodexReasoningReplayOffKey] != "off",
		currentIssuerKind:  classifyResponsesIssuer(c.flags.isXAI, c.flags.isCodexBackend, c.route.BaseURL, &endpoint),
		currentIssuerModel: wireModelIdentity(model), nativeCompactionEligible: true,
	})
	if err != nil {
		slog.Default().Debug("native Responses replay conversion failed; using the generic fallback", "error", err)
		return 0, false
	}
	total := 0
	if systemPrompt != "" {
		total = EstimateTokens(systemPrompt)
	}
	if len(items) > 0 {
		total += estimateRowsTokens(items, CurrentImageTokenCost(ctx))
	}
	if len(tools) > 0 {
		total += EstimateToolsTokens(tools)
	}
	return total, true
}

// Builds, preflights and serializes one request with its headers and watchdogs
func (c *codexResponsesClient) prepare(ctx context.Context, req *Request) (*codexPrepared, error) {
	if req == nil {
		return nil, errors.New("provider: codex responses request is nil")
	}
	if IsActualRoute(c.route.Provider, c.route.BaseURL) {
		return nil, errors.New("Actual requests require Chat Completions; refusing to call /responses.")
	}
	settings := loadWireSettings(ctx)
	timeout, _ := c.requestTimeout(req, settings)
	model := req.Model
	if model == "" {
		model = c.route.Model
	}
	endpoint := c.flags.endpoint
	contextManagement := c.nativeContextManagement(ctx, req, model, settings, &endpoint)
	var profileReasoning map[string]any
	if endpoint.profileReasoning {
		if p, ok := profileForHost(c.route.BaseURL); ok {
			profileReasoning = p.ReasoningPayload(model, req.Reasoning, c.route.BaseURL)
		}
	}
	params := codexParams{
		ctx: ctx, reasoning: reasoningFromRequest(req.Reasoning), sessionID: req.SessionID, cacheScopeID: req.CacheScope,
		maxTokens: req.MaxTokens, timeout: timeout.Seconds(), requestOverrides: c.requestOverrides(req),
		provider: c.route.Provider, baseURL: c.route.BaseURL, isCodexBackend: c.flags.isCodexBackend, isXAI: c.flags.isXAI,
		profileReasoning: profileReasoning, replayEncryptedReasoning: req.Metadata[CodexReasoningReplayOffKey] != "off",
		contextManagement: contextManagement, textVerbosity: settings.textVerbosity,
		webSearchBackend: func() (string, error) { return activeWebSearchBackend(ctx) },
	}
	kwargs, err := c.transport.buildKwargs(model, req.Messages, req.Tools, params)
	if err != nil {
		return nil, Classify(c.route.Provider, c.route.Model, 0, "", &LocalValidationError{Err: err})
	}
	kwargs, err = c.transport.preflightKwargs(kwargs, false, dropsReplayItemIDs(c.route.BaseURL), c.flags.isCodexBackend)
	if err != nil {
		return nil, Classify(c.route.Provider, c.route.Model, 0, "", &LocalValidationError{Err: err})
	}
	kwargs = c.sanitizeConsumerRequest(kwargs)
	wire := make(map[string]any, len(kwargs)+1)
	for k, v := range kwargs {
		if k == "extra_headers" || k == "extra_body" || k == "timeout" {
			continue
		}
		wire[k] = v
	}
	if extraBody, ok := kwargs["extra_body"].(map[string]any); ok {
		for k, v := range extraBody {
			wire[k] = v
		}
	}
	wire["stream"] = true
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("provider: encode codex responses body: %w", err)
	}
	endpointURL, err := ccEndpointURL(c.route, "/responses")
	if err != nil {
		return nil, err
	}
	headers, err := c.requestHeaders(ctx, kwargs)
	if err != nil {
		return nil, err
	}
	if t, ok := kwargs["timeout"].(float64); ok && t > 0 {
		timeout = secondsDuration(t)
	}
	est := EstimateRequestContextTokens(ctx, wire)
	return &codexPrepared{kwargs: kwargs, body: body, wire: wire, url: endpointURL, headers: headers, settings: settings,
		estTokens: est, timeout: timeout, watch: c.resolveWatchdogs(ctx, req, settings, est, kwargs)}, nil
}

// Drops prompt_cache_retention at the ChatGPT Codex wire boundary
func (c *codexResponsesClient) sanitizeConsumerRequest(kwargs map[string]any) map[string]any {
	if !c.flags.isCodexBackend {
		return kwargs
	}
	sanitized := make(map[string]any, len(kwargs))
	for k, v := range kwargs {
		sanitized[k] = v
	}
	var droppedFrom []string
	if _, has := sanitized["prompt_cache_retention"]; has {
		droppedFrom = append(droppedFrom, "top-level")
		delete(sanitized, "prompt_cache_retention")
	}
	if extraBody, ok := sanitized["extra_body"].(map[string]any); ok {
		if _, has := extraBody["prompt_cache_retention"]; has {
			cp := map[string]any{}
			for k, v := range extraBody {
				if k != "prompt_cache_retention" {
					cp[k] = v
				}
			}
			if len(cp) == 0 {
				delete(sanitized, "extra_body")
			} else {
				sanitized["extra_body"] = cp
			}
			droppedFrom = append(droppedFrom, "extra_body")
		}
	}
	if len(droppedFrom) > 0 {
		codexLog.Warn(fmt.Sprintf("Dropped unsupported prompt_cache_retention at consumer Codex wire boundary (model=%s, via %s).", orUnknown2(strOrEmpty(sanitized["model"])), strings.Join(droppedFrom, ", ")))
	}
	return sanitized
}

// Identity, account, per request and auth headers of one request
func (c *codexResponsesClient) requestHeaders(ctx context.Context, kwargs map[string]any) (map[string]string, error) {
	headers := map[string]string{"User-Agent": userAgent("IcebathAgent")}
	if c.flags.isXAI {
		headers["User-Agent"] = userAgent("Icebath-Agent")
	}
	accessToken := c.route.APIKey
	if c.flags.isCodexBackend {
		for k, v := range codexCloudflareHeaders(accessToken, c.route.BaseURL) {
			headers[k] = v
		}
	}
	for k, v := range c.route.Headers {
		headers[k] = v
	}
	if eh, ok := kwargs["extra_headers"].(map[string]string); ok {
		for k, v := range eh {
			headers[k] = v
		}
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
	if c.flags.isCodexBackend {
		token := accessToken
		if bearer := strings.TrimPrefix(auth["Authorization"], "Bearer "); bearer != "" {
			token = bearer
		}
		headers = applyRequiredCodexHeaders(headers, token, c.route.BaseURL)
	}
	return headers, nil
}

func (c *codexResponsesClient) newRequest(ctx context.Context, p *codexPrepared, body []byte) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	for k, v := range p.headers {
		httpReq.Header.Set(k, v)
	}
	codexCompressRequest(httpReq, c.route.BaseURL, body)
	return httpReq, nil
}

// APIError for a non success HTTP response
func (c *codexResponsesClient) httpError(resp *http.Response, data []byte) error {
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

// Complete runs one streamed Responses call to completion without a consumer
func (c *codexResponsesClient) Complete(ctx context.Context, req *Request) (*Response, error) {
	return c.Stream(ctx, req, nil)
}

// CountTokens estimates the context the request occupies on this wire
func (c *codexResponsesClient) CountTokens(ctx context.Context, req *Request) (int, error) {
	p, err := c.prepare(ctx, req)
	if err != nil {
		return 0, err
	}
	return p.estTokens, nil
}

// Reports whether an attempt failed on the transport rather than on a provider verdict
func codexTransportFailure(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return false
	}
	var kill *codexKillError
	if errors.As(err, &kill) {
		return false
	}
	var pse *ProviderStreamError
	if errors.As(err, &pse) {
		return false
	}
	var se *CodexStreamErrorEvent
	if errors.As(err, &se) {
		return false
	}
	if errors.Is(err, errCodexNoTerminal) {
		return false
	}
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

// Stream issues one streamed Responses call with one physical retry before the stream opens or on a cut transport
func (c *codexResponsesClient) Stream(ctx context.Context, req *Request, fn StreamFunc) (*Response, error) {
	p, err := c.prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	const maxStreamRetries = 1
	kwargs := p.kwargs
	watch := &codexWatchState{callStart: time.Now()}
	watch.waiter = newNonStreamWaiter(req.Model, epochSeconds(watch.callStart), p.watch, nonStreamEmitter(ctx, fn))
	watch.waiter.touch("waiting for non-streaming API response")
	for attempt := 0; attempt <= maxStreamRetries; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		resp, events, err := c.streamOnce(ctx, req, p, kwargs, attempt, watch, fn)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt < maxStreamRetries && codexTransportFailure(err) {
			codexLog.Debug(fmt.Sprintf("Codex Responses stream transport failed (attempt %d/%d); retrying. model=%s error=%v", attempt+1, maxStreamRetries+1, orUnknown2(c.route.Model), err))
			if events == 0 {
				kwargs = pruneZeroEventRetryPayload(ctx, kwargs, attempt+1, maxStreamRetries+1)
				if body, encErr := json.Marshal(c.wireOf(kwargs)); encErr == nil {
					p.body = body
				}
			}
			continue
		}
		return nil, c.attemptError(err, p)
	}
	return nil, errors.New("provider: codex stream attempt loop ended without a response")
}

// Wire body of request kwargs with extras merged and the stream flag set
func (c *codexResponsesClient) wireOf(kwargs map[string]any) map[string]any {
	wire := make(map[string]any, len(kwargs)+1)
	for k, v := range kwargs {
		if k == "extra_headers" || k == "extra_body" || k == "timeout" {
			continue
		}
		wire[k] = v
	}
	if extraBody, ok := kwargs["extra_body"].(map[string]any); ok {
		for k, v := range extraBody {
			wire[k] = v
		}
	}
	wire["stream"] = true
	return wire
}

// Watchdog kill of one stream attempt with the status line already emitted
type codexKillError struct {
	reason  string
	message string
	headers http.Header
}

func (e *codexKillError) Error() string { return e.message }

// APIError of a failed attempt, transport failures classified with their phase
func (c *codexResponsesClient) attemptError(err error, p *codexPrepared) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return err
	}
	var kill *codexKillError
	if errors.As(err, &kill) {
		base := &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: &ccTimeoutError{err: errors.New(kill.message), headers: kill.headers}}
		return Classify(c.route.Provider, c.route.Model, 0, "", base)
	}
	var se *CodexStreamErrorEvent
	if errors.As(err, &se) {
		out := Classify(c.route.Provider, c.route.Model, se.StatusCode, se.BodyText(), se)
		if se.Code != "" && out.Code == "" {
			out.Code = se.Code
		}
		if out.Message == "" {
			out.Message = se.Message
		}
		return out
	}
	var pse *ProviderStreamError
	if errors.As(err, &pse) {
		out := Classify(c.route.Provider, c.route.Model, pse.StatusCode, pse.BodyText(), pse)
		if code := pse.Code(); code != "" {
			out.Code = code
		}
		if out.Message == "" {
			out.Message = pse.Error()
		}
		if pse.Headers != nil && out.RetryAfter == 0 {
			out.RetryAfter = RetryAfterFrom(pse.Headers.Get("Retry-After"))
		}
		return out
	}
	if errors.Is(err, errCodexNoTerminal) {
		return &EmptyStreamError{Message: errCodexNoTerminal.Error()}
	}
	return Classify(c.route.Provider, c.route.Model, 0, "", ccWrapTransport(err, TimeoutPhaseConnect))
}

type codexFrameMsg struct {
	frame *sseFrame
	err   error
}

// Watchdog clocks shared by the physical attempts of one call
type codexWatchState struct {
	callStart    time.Time
	attemptStart time.Time
	lastEvent    time.Time
	lastProgress time.Time
	retryStarted time.Time
	waiter       *nonStreamWaiter
}

// Watchdog markers of the current attempt in epoch seconds
func (w *codexWatchState) snapshot() nonStreamSnapshot {
	return nonStreamSnapshot{lastEvent: markerSeconds(w.lastEvent), lastProgress: markerSeconds(w.lastProgress), retryStarted: markerSeconds(w.retryStarted)}
}

// Runs one physical streaming attempt, returning the response, the events parsed and the failure
func (c *codexResponsesClient) streamOnce(ctx context.Context, req *Request, p *codexPrepared, kwargs map[string]any, attempt int, watch *codexWatchState, fn StreamFunc) (*Response, int, error) {
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if p.timeout > 0 {
		var tcancel context.CancelFunc
		attemptCtx, tcancel = context.WithTimeout(attemptCtx, p.timeout)
		defer tcancel()
	}
	connect := 30 * time.Second
	if p.timeout > 0 {
		connect = p.timeout
		if connect > 60*time.Second {
			connect = 60 * time.Second
		}
	}
	start := time.Now()
	if attempt == 0 || p.watch.idleRequiresProgress {
		watch.attemptStart = start
		watch.lastEvent, watch.lastProgress = time.Time{}, time.Time{}
	}
	if attempt > 0 && p.watch.idleRequiresProgress {
		watch.retryStarted = start
		codexLog.Info(fmt.Sprintf("Codex physical stream retry at %.3f (attempt=%d/%d, model=%s)", float64(start.UnixNano())/1e9, attempt+1, 2, orUnknown2(req.Model)))
	}
	model := orUnknown2(req.Model)
	httpReq, err := c.newRequest(attemptCtx, p, p.body)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.httpClient(connect).Do(httpReq)
	if err != nil {
		if ctx.Err() == nil && errors.Is(attemptCtx.Err(), context.DeadlineExceeded) {
			return nil, 0, &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: fmt.Errorf("Request timed out after %.0fs.", p.timeout.Seconds())}
		}
		return nil, 0, err
	}
	defer resp.Body.Close()
	headers := resp.Header.Clone()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := readLimited(resp.Body, ccErrorBodyLimit)
		return nil, 0, c.httpError(resp, data)
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/json") && !strings.Contains(ct, "event-stream") {
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, 0, err
		}
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			return nil, 0, providerStreamErrorFromNonJSON(string(data), headers)
		}
		if eo, ok := payload["error"]; ok && eo != nil {
			if em, ok := eo.(map[string]any); !ok || len(em) > 0 {
				return nil, 0, c.httpError(resp, data)
			}
		}
		if !validateCodexResponse(payload) {
			return nil, 0, &EmptyStreamError{Message: "Responses API returned no output items"}
		}
		final, err := c.finish(payload, resp.StatusCode, headers, time.Since(start), fn, nil)
		return final, 1, err
	}
	emit := func(ev StreamEvent) error {
		if fn == nil {
			return nil
		}
		return fn(ev)
	}
	assembler := newCodexAssembler(model)
	assembler.textFn = func(text string) error { return emit(StreamEvent{Kind: EventTextDelta, Text: text}) }
	assembler.reasoningFn = func(text string) error { return emit(StreamEvent{Kind: EventReasoningDelta, Text: text}) }
	toolStarts := 0
	assembler.toolStartFn = func(name string) error {
		idx := toolStarts
		toolStarts++
		return emit(StreamEvent{Kind: EventToolCallStart, Index: idx, ToolCall: &state.ToolCall{Type: "function", Function: state.ToolCallFunction{Name: name}}})
	}
	frames := make(chan codexFrameMsg, 32)
	go func() {
		r := newSSEReader(resp.Body)
		for {
			f, err := r.Next()
			select {
			case frames <- codexFrameMsg{frame: f, err: err}:
			case <-attemptCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	w := p.watch
	events := 0
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	kill := func(reason, status, message string) error {
		watch.waiter.surface(StreamEvent{Kind: EventStatus, Text: status})
		cancel()
		return &codexKillError{reason: reason, message: message, headers: headers}
	}
	hint := codexSilentHangHint(c.flags.isCodexBackend, req.Model)
	withHint := func(message, sep string) string {
		if hint != "" {
			return message + sep + hint
		}
		return message
	}
	terminal := false
	for !terminal {
		select {
		case <-ctx.Done():
			recordInterruptedProviderWait(ctx, time.Since(watch.callStart).Seconds(), !watch.lastEvent.IsZero())
			codexLog.Debug("Force-closing httpx client due to interrupt (not a network error).")
			return nil, events, ctx.Err()
		case <-attemptCtx.Done():
			if ctx.Err() == nil && errors.Is(attemptCtx.Err(), context.DeadlineExceeded) {
				return nil, events, &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: fmt.Errorf("Request timed out after %.0fs.", p.timeout.Seconds())}
			}
			return nil, events, attemptCtx.Err()
		case now := <-ticker.C:
			watch.waiter.poll(epochSeconds(now), watch.snapshot())
			elapsed := now.Sub(watch.callStart).Seconds()
			attemptElapsed := now.Sub(watch.attemptStart).Seconds()
			lastEvent, lastProgress := watch.lastEvent, watch.lastProgress
			if w.ttfbEnabled && lastEvent.IsZero() && attemptElapsed > w.ttfb {
				codexLog.Warn(fmt.Sprintf("Codex stream produced no parsed stream event within TTFB cutoff (%.0fs > %.0fs, model=%s). Backend accepted the connection but sent no stream events. Killing connection so the retry loop can reconnect.", attemptElapsed, w.ttfb, model))
				return nil, events, kill("codex_ttfb_kill",
					withHint(fmt.Sprintf("⚠️ No first stream event from provider in %ds (codex stream, model: %s). Reconnecting.", int(attemptElapsed), model), " "),
					withHint(fmt.Sprintf("Codex stream produced no parsed stream event within %ds (TTFB threshold: %ds)", int(attemptElapsed), int(w.ttfb)), ". "))
			}
			if w.progress > 0 && !lastEvent.IsZero() && lastProgress.IsZero() && attemptElapsed > w.progress {
				codexLog.Warn(fmt.Sprintf("Codex stream produced lifecycle events but no substantive model progress for %.0fs (threshold %.0fs, model=%s, context=~%s tokens). Reconnecting.", attemptElapsed, w.progress, model, commaInt(w.estTokens)))
				return nil, events, kill("codex_progress_kill",
					fmt.Sprintf("⚠️ Codex stream opened but made no model progress for %ds (model: %s). Reconnecting.", int(attemptElapsed), model),
					fmt.Sprintf("Codex stream produced no substantive model progress for %ds (progress threshold: %ds)", int(attemptElapsed), int(w.progress)))
			}
			if w.idleEnabled && !lastEvent.IsZero() && (!w.idleRequiresProgress || !lastProgress.IsZero()) {
				idle := now.Sub(lastEvent).Seconds()
				if idle > w.idle {
					armPoint := "the first parsed event"
					if w.idleRequiresProgress {
						armPoint = "model progress began"
					}
					codexLog.Warn(fmt.Sprintf("Codex stream produced no SSE events for %.0fs after %s (threshold %.0fs, model=%s, context=~%s tokens). Killing connection so the retry loop can reconnect.", idle, armPoint, w.idle, model, commaInt(w.estTokens)))
					return nil, events, kill("codex_stream_idle_kill",
						fmt.Sprintf("⚠️ Codex stream sent no events for %ds after %s (model: %s). Reconnecting.", int(idle), armPoint, model),
						fmt.Sprintf("Codex stream produced no SSE events for %ds after %s (threshold: %ds)", int(idle), armPoint, int(w.idle)))
				}
			}
			if elapsed > w.stale {
				codexLog.Warn(fmt.Sprintf("Non-streaming API call stale for %.0fs (threshold %.0fs). model=%s context=~%s tokens. Killing connection.", elapsed, w.stale, model, commaInt(w.estTokens)))
				status := "Aborting call."
				if hint != "" {
					status = hint
				}
				bumpStaleStreak(ctx)
				return nil, events, kill("stale_call_kill",
					fmt.Sprintf("⚠️ No response from provider for %ds (non-streaming, model: %s). %s", int(elapsed), model, status),
					withHint(fmt.Sprintf("Non-streaming API call timed out after %ds with no response (threshold: %ds)", int(elapsed), int(w.stale)), ". "))
			}
		case msg := <-frames:
			if msg.err != nil {
				if errors.Is(msg.err, io.EOF) {
					result, rerr := assembler.result()
					if rerr != nil {
						return nil, events, rerr
					}
					final, err := c.finish(result, resp.StatusCode, headers, time.Since(start), fn, assembler)
					return final, events, err
				}
				return nil, events, msg.err
			}
			f := msg.frame
			if f == nil || (f.Data == "" && f.Event == "") {
				continue
			}
			if strings.TrimSpace(f.Data) == "" {
				if f.Event != "" {
					continue
				}
				return nil, events, providerStreamErrorFromNonJSON("", headers)
			}
			if strings.TrimSpace(f.Data) == "[DONE]" {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(f.Data), &event); err != nil || event == nil {
				return nil, events, providerStreamErrorFromNonJSON(f.Data, headers)
			}
			if _, has := event["type"]; !has && f.Event != "" {
				event["type"] = f.Event
			}
			now := time.Now()
			if watch.lastEvent.IsZero() {
				codexLog.Info(fmt.Sprintf("Codex stream first parsed event at %.3f (attempt=%d/%d, model=%s)", float64(now.UnixNano())/1e9, attempt+1, 2, model))
			}
			watch.lastEvent = now
			events++
			if codexEventHasContent(event) {
				if watch.lastProgress.IsZero() {
					codexLog.Info(fmt.Sprintf("Codex stream first substantive progress at %.3f (attempt=%d/%d, model=%s)", float64(now.UnixNano())/1e9, attempt+1, 2, model))
				}
				watch.lastProgress = now
				if p.watch.idleRequiresProgress {
					watch.retryStarted = time.Time{}
				}
			}
			done, err := assembler.feed(event)
			if err != nil {
				return nil, events, err
			}
			terminal = done
		}
	}
	c.drain(frames, p.settings.streamDrainTimeout, model)
	result, err := assembler.result()
	if err != nil {
		return nil, events, err
	}
	if status := stringOf(result["status"]); status == "incomplete" || status == "failed" {
		codexLog.Warn(fmt.Sprintf("Codex Responses stream terminal status=%s (incomplete_details=%v, error=%v, streamed_chars=%d).", status, result["incomplete_details"], result["error"], len(strOrEmpty(result["output_text"]))))
	}
	final, err := c.finish(result, resp.StatusCode, headers, time.Since(start), fn, assembler)
	return final, events, err
}

// Drains frames after the terminal event for the configured budget so a sticky relay cannot wedge the turn
func (c *codexResponsesClient) drain(frames <-chan codexFrameMsg, budget float64, model string) {
	if budget <= 0 {
		return
	}
	timer := time.NewTimer(secondsDuration(budget))
	defer timer.Stop()
	for {
		select {
		case msg := <-frames:
			if msg.err != nil {
				return
			}
		case <-timer.C:
			codexLog.Warn(fmt.Sprintf("Codex Responses stream remained open %.1fs after a terminal response (agent.stream_drain_timeout); shut down its socket and completed cleanup on the owner thread. model=%s", budget, model))
			return
		}
	}
}

// Response of an assembled Responses object with the trailing tool call, usage and done events
func (c *codexResponsesClient) finish(result map[string]any, status int, headers http.Header, latency time.Duration, fn StreamFunc, assembler *codexAssembler) (*Response, error) {
	msg, finish, err := c.transport.normalizeResponse(result, "", "")
	if err != nil {
		return nil, &EmptyStreamError{Message: err.Error()}
	}
	usage := Usage{}
	if u, ok := result["usage"].(map[string]any); ok {
		usage = codexUsageFromMap(u)
	}
	model := stringOf(result["model"])
	if model == "" {
		model = c.route.Model
	}
	raw, _ := json.Marshal(result)
	out := &Response{Message: msg, FinishReason: finish, Usage: usage, Model: model, Provider: c.route.Provider,
		RequestID: firstNonEmpty(stringOf(result["id"]), headerRequestID(headers)), StatusCode: status, Headers: headers, Raw: raw,
		Latency: latency, Truncated: stringOf(result["status"]) == "incomplete"}
	if fn == nil {
		return out, nil
	}
	if assembler == nil {
		if msg.Reasoning != "" {
			if err := fn(StreamEvent{Kind: EventReasoningDelta, Text: msg.Reasoning}); err != nil {
				return out, err
			}
		}
		if text := msg.Text(); text != "" {
			if err := fn(StreamEvent{Kind: EventTextDelta, Text: text}); err != nil {
				return out, err
			}
		}
	}
	for i := range msg.ToolCalls {
		tc := msg.ToolCalls[i]
		if err := fn(StreamEvent{Kind: EventToolCallDone, Index: i, ToolCall: &tc}); err != nil {
			return out, err
		}
	}
	if err := fn(StreamEvent{Kind: EventUsage, Usage: &usage}); err != nil {
		return out, err
	}
	return out, fn(StreamEvent{Kind: EventDone})
}

// NativePreflightEstimator estimates the natively compacted Responses wire for the preflight compression check
type NativePreflightEstimator interface {
	EstimateNativePreflightTokens(ctx context.Context, req *Request, systemPrompt string, tools []map[string]any) (int, bool)
}

var _ NativePreflightEstimator = (*codexResponsesClient)(nil)
