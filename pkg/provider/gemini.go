package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Registers the native Gemini adapter and routes native Gemini base URLs of chat routes onto it
func init() {
	RegisterFactory(ModeGeminiNative, func(route Route) (Client, error) { return newGeminiNativeClient(route) })
	RegisterFactory(ModeChatCompletions, func(route Route) (Client, error) {
		if isNativeGeminiBaseURL(route.BaseURL) {
			return newGeminiNativeClient(route)
		}
		return newChatCompletionsClient(route)
	})
}

const (
	geminiFreeTierGuidance = "\n\nYour Google API key is on the free tier (a few hundred requests/day for Gemini Flash models). " +
		"Icebath typically makes 3-10 API calls per user turn, so the free tier is exhausted in a handful of " +
		"messages and cannot sustain an agent session. Enable billing on your Google Cloud project and " +
		"regenerate the key in a billing-enabled project: https://aistudio.google.com/apikey"
	geminiStandardKeyGuidance = "\n\nGoogle Gemini rejected this API key's type — you do NOT need OAuth. Google began rejecting legacy " +
		"'Standard' Google Cloud keys for the Gemini API on June 19, 2026, and all Standard keys stop working in " +
		"September 2026. Open https://aistudio.google.com/api-keys, check the key's type and status, and create a " +
		"replacement Gemini API key (or, as a temporary bridge, restrict the Standard key to " +
		"generativelanguage.googleapis.com). Then update GEMINI_API_KEY / GOOGLE_API_KEY in ~/.icebath/.env, " +
		"delete any leftover Windows/system copy of those variables, and restart. A stale shell key can hide " +
		"the .env value. Details: https://ai.google.dev/gemini-api/docs/api-key"
	geminiExpressKeyOnStudioGuidance = "\n\nGoogle issues 'AQ.' keys for both Google AI Studio and Vertex AI express mode, and each surface " +
		"only accepts its own keys: a Vertex express key always gets 403 PERMISSION_DENIED on " +
		"generativelanguage.googleapis.com. If this key came from Google Cloud / Vertex (express mode), point " +
		"the provider at the express surface, e.g. set base_url / GEMINI_BASE_URL to " +
		"https://aiplatform.googleapis.com/v1beta1."
	geminiStudioKeyOnExpressGuidance = "\n\nIf this 'AQ.' key came from Google AI Studio (https://aistudio.google.com/apikey), it only works " +
		"on the default generativelanguage.googleapis.com host — remove the explicit aiplatform base_url so " +
		"the default surface is used."
	geminiMissingKeyError = "Gemini native client requires an API key, but none was provided. Set GOOGLE_API_KEY or GEMINI_API_KEY in your " +
		"environment / ~/.icebath/.env (get one at https://aistudio.google.com/app/apikey), or run `icebath setup` to " +
		"configure the Google provider."
	geminiVertexCredentialsError = "Vertex AI credentials could not be resolved for the native Gemini route. Vertex uses OAuth2 (not a static API key): " +
		"provide a service-account JSON via GOOGLE_APPLICATION_CREDENTIALS (or VERTEX_CREDENTIALS_PATH) in ~/.icebath/.env, " +
		"or run 'gcloud auth application-default login' for ADC."
	geminiGenerateMethod = "generateContent"
	geminiStreamMethod   = "streamGenerateContent"
	// Requests one long decode token may ride, so a network that keeps dropping ends in an error
	geminiMaxTokenSends = 3
)

var geminiHTTPErrorCodes = map[int]string{401: "gemini_unauthorized", 429: "gemini_rate_limited", 404: "gemini_model_not_found"}

var geminiAPIVersionSegment = regexp.MustCompile(`(?i)^v\d+(?:alpha|beta)?\d*$`)

var geminiOpenAISuffix = regexp.MustCompile(`(?i)/openai$`)

// Client context header value per the Gemini partner integration guidance
func geminiAPIClient() string { return userAgent("icebath-agent") }

// Reports whether an aiplatform base speaks the native API with an API key rather than the OpenAI endpoints
func isVertexExpressGeminiBaseURL(baseURL string) bool {
	n := strings.ToLower(strings.TrimSpace(baseURL))
	return strings.Contains(n, "aiplatform.googleapis.com") && !strings.Contains(n, "/endpoints/")
}

// Gemini native base URL with its API version segment, express bases completed to publishers/google
func normalizeGeminiBaseURL(baseURL string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	trimmed = strings.TrimRight(geminiOpenAISuffix.ReplaceAllString(trimmed, ""), "/")
	if trimmed == "" {
		trimmed = defaultGeminiBaseURL
	}
	last := trimmed[strings.LastIndex(trimmed, "/")+1:]
	if isVertexExpressGeminiBaseURL(trimmed) {
		lower := strings.ToLower(trimmed)
		if strings.HasSuffix(lower, "/publishers/google") {
			return trimmed
		}
		if !strings.Contains(lower, "/projects/") && !geminiAPIVersionSegment.MatchString(last) {
			trimmed += "/v1beta1"
		}
		return trimmed + "/publishers/google"
	}
	if geminiAPIVersionSegment.MatchString(last) {
		return trimmed
	}
	return trimmed + "/v1beta"
}

// GeminiDefaultMaxOutputTokens is the maxOutputTokens the native adapter sends when no cap is set
const GeminiDefaultMaxOutputTokens = geminiDefaultMaxOutputTokens

// IsNativeGeminiBaseURL reports whether a base URL is served by the native Gemini adapter
func IsNativeGeminiBaseURL(baseURL string) bool { return isNativeGeminiBaseURL(baseURL) }

// Reports whether the endpoint speaks the native Gemini REST API rather than its OpenAI surface
func isNativeGeminiBaseURL(baseURL string) bool {
	n := strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if strings.HasSuffix(n, "/openai") {
		return false
	}
	return strings.Contains(n, "generativelanguage.googleapis.com") || isVertexExpressGeminiBaseURL(n)
}

// Reports whether the base is the v1beta surface that has parametersJsonSchema
func geminiAcceptsParametersJSONSchema(baseURL string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/")), "/v1beta")
}

// Vertex OpenAI endpoint base rewritten to the native publishers/google path
func vertexNativeGeminiBase(baseURL string) string {
	if i := strings.Index(strings.ToLower(baseURL), "/endpoints/"); i >= 0 && strings.Contains(strings.ToLower(baseURL), "aiplatform.googleapis.com") {
		return baseURL[:i] + "/publishers/google"
	}
	return baseURL
}

// Tier of a Google AI Studio key as free, paid, or unknown when the probe fails
func probeGeminiTier(ctx context.Context, apiKey, baseURL, model string, timeout time.Duration) string {
	key := strings.TrimSpace(apiKey)
	if key == "" {
		return "unknown"
	}
	if baseURL == "" {
		baseURL = defaultGeminiBaseURL
	}
	if model == "" {
		model = "gemini-3.7-flash"
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	payload := []byte(`{"contents": [{"role": "user", "parts": [{"text": "hi"}]}], "generationConfig": {"maxOutputTokens": 1}}`)
	endpoint := normalizeGeminiBaseURL(baseURL) + "/models/" + model + ":generateContent?key=" + urlQueryEscape(key)
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		catalogLog.Debug("probe_gemini_tier: network error: " + err.Error())
		return "unknown"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-Api-Client", geminiAPIClient())
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: PlatformTLSConfig()}}).Do(req)
	if err != nil {
		catalogLog.Debug("probe_gemini_tier: network error: " + err.Error())
		return "unknown"
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if rpd := resp.Header.Get("x-ratelimit-limit-requests-per-day"); rpd != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(rpd)); err == nil {
			if n <= 1000 {
				return "free"
			}
			return "paid"
		}
	}
	if resp.StatusCode == 429 {
		if strings.Contains(strings.ToLower(string(body)), "free_tier") {
			return "free"
		}
		return "paid"
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return "paid"
	}
	return "unknown"
}

// Query escaping for the probe key parameter
func urlQueryEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Reports whether a Gemini 429 message names free tier exhaustion
func isFreeTierQuotaError(errorMessage string) bool {
	return errorMessage != "" && strings.Contains(strings.ToLower(errorMessage), "free_tier")
}

// Guidance for a PERMISSION_DENIED where the AQ key and the host surface are crossed
func wrongGeminiSurfaceGuidance(baseURL, apiKey, errStatus string) string {
	if strings.ToUpper(strings.TrimSpace(errStatus)) != "PERMISSION_DENIED" {
		return ""
	}
	n := strings.ToLower(strings.TrimSpace(baseURL))
	if isVertexExpressGeminiBaseURL(n) {
		return geminiStudioKeyOnExpressGuidance
	}
	if strings.Contains(n, "generativelanguage.googleapis.com") && strings.HasPrefix(strings.ToUpper(strings.TrimSpace(apiKey)), "AQ.") {
		return geminiExpressKeyOnStudioGuidance
	}
	return ""
}

// Reports whether Google rejected a legacy Standard AIza Cloud key
func isStandardKeyAuthError(status int, errorMessage, reason, apiKey string) bool {
	msg := strings.ToLower(errorMessage)
	if status == 401 && (reason == "ACCESS_TOKEN_TYPE_UNSUPPORTED" || strings.Contains(msg, "expected oauth 2 access token")) {
		return true
	}
	return status == 400 && strings.HasPrefix(apiKey, "AIza") && (reason == "API_KEY_INVALID" || strings.Contains(msg, "api key not valid"))
}

// GeminiAPIError is a failed native Gemini call with its google.rpc status, reason and guidance
type GeminiAPIError struct {
	Message    string
	Code       string
	StatusCode int
	RetryAfter time.Duration
	Details    map[string]any
	Headers    http.Header
	Body       string
	Err        error
}

func (e *GeminiAPIError) Error() string { return e.Message }

func (e *GeminiAPIError) Unwrap() error { return e.Err }

// Response headers of the failed call for retry after parsing
func (e *GeminiAPIError) ResponseHeaders() http.Header { return e.Headers }

// Reason and metadata of the first google.rpc.ErrorInfo detail
func geminiErrorInfo(errObj map[string]any) (string, map[string]any) {
	reason, metadata := "", map[string]any{}
	details, _ := errObj["details"].([]any)
	for _, raw := range details {
		d, ok := raw.(map[string]any)
		if !ok || reason != "" || !strings.HasSuffix(pyOrStr(d["@type"]), "/google.rpc.ErrorInfo") {
			continue
		}
		if r, ok := d["reason"].(string); ok {
			reason = r
		}
		if m, ok := d["metadata"].(map[string]any); ok {
			metadata = m
		}
	}
	return reason, metadata
}

// error object of a Google JSON error body, empty when absent
func geminiErrorObject(bodyText string) map[string]any {
	if bodyText == "" {
		return map[string]any{}
	}
	parsed, err := geminiJSONLoads(bodyText)
	if err != nil {
		return map[string]any{}
	}
	pm, _ := parsed.(map[string]any)
	eo, ok := pm["error"].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return eo
}

// GeminiAPIError for a non 200 response with free tier, Standard key and key surface guidance
func geminiHTTPError(status int, headers http.Header, bodyText, apiKey, baseURL string, keyGuidance bool) *GeminiAPIError {
	errObj := geminiErrorObject(bodyText)
	errStatus := strings.TrimSpace(pyOrStr(errObj["status"]))
	errMessage := strings.TrimSpace(pyOrStr(errObj["message"]))
	reason, metadata := geminiErrorInfo(errObj)
	retryAfter, _ := RetryAfterFromHeaders(headers)
	var message string
	if errMessage != "" {
		label := errStatus
		if label == "" {
			label = "error"
		}
		message = fmt.Sprintf("Gemini HTTP %d (%s): %s", status, label, errMessage)
	} else {
		message = fmt.Sprintf("Gemini returned HTTP %d: %s", status, geminiTruncateRunes(bodyText, 500))
	}
	probe := errMessage
	if probe == "" {
		probe = bodyText
	}
	if keyGuidance && status == 429 && isFreeTierQuotaError(probe) {
		message += geminiFreeTierGuidance
	}
	if keyGuidance && isStandardKeyAuthError(status, probe, reason, apiKey) {
		message += geminiStandardKeyGuidance
	}
	if keyGuidance && status == 403 {
		message += wrongGeminiSurfaceGuidance(baseURL, apiKey, errStatus)
	}
	code, ok := geminiHTTPErrorCodes[status]
	if !ok {
		code = fmt.Sprintf("gemini_http_%d", status)
	}
	return &GeminiAPIError{Message: message, Code: code, StatusCode: status, RetryAfter: retryAfter, Headers: headers, Body: bodyText,
		Details: map[string]any{"status": errStatus, "reason": reason, "metadata": metadata, "message": errMessage}}
}

// Client speaking the native Gemini generateContent wire on top of the chat completions request builder
type geminiNativeClient struct {
	route  Route
	chat   *chatCompletionsClient
	bearer bool
	mu     sync.Mutex
	base   string
	token  func(context.Context) (string, error)
}

func newGeminiNativeClient(route Route) (Client, error) {
	c := &geminiNativeClient{route: route, token: route.TokenSource,
		chat:   &chatCompletionsClient{route: route, cc: newCCRoute(route), transports: map[time.Duration]*http.Transport{}},
		bearer: route.TokenSource != nil || route.Auth == AuthGCPOAuth}
	if !c.bearer && strings.TrimSpace(route.APIKey) == "" {
		return nil, errors.New(geminiMissingKeyError)
	}
	if !c.bearer || strings.TrimSpace(route.BaseURL) != "" {
		c.base = normalizeGeminiBaseURL(vertexNativeGeminiBase(route.BaseURL))
	}
	return c, nil
}

func (c *geminiNativeClient) Route() Route { return c.route }

// Base URL and credential headers, minting the Vertex token source from the home config when the route has none
func (c *geminiNativeClient) credentials(ctx context.Context) (string, map[string]string, error) {
	if !c.bearer {
		return c.base, map[string]string{"x-goog-api-key": c.route.APIKey}, nil
	}
	c.mu.Lock()
	src, base := c.token, c.base
	c.mu.Unlock()
	if src == nil || base == "" {
		h := home.Dir(ctx)
		secrets := config.NewSecrets(h)
		tree, err := config.Load(h, secrets.Lookup())
		if err != nil {
			tree = config.Defaults()
		}
		tokenSrc, vertexBase, ok := vertexRuntimeConfig(ctx, map[string]any(tree), secrets, h)
		if !ok || tokenSrc == nil || (base == "" && vertexBase == "") {
			return "", nil, &APIError{Provider: c.route.Provider, Model: c.route.Model, StatusCode: 401, Kind: ErrAuth, Code: "vertex_credentials_missing",
				Message: geminiVertexCredentialsError, Err: errors.New(geminiVertexCredentialsError)}
		}
		if src == nil {
			src = tokenSrc
		}
		if base == "" {
			base = normalizeGeminiBaseURL(vertexNativeGeminiBase(vertexBase))
		}
		c.mu.Lock()
		c.token, c.base = src, base
		c.mu.Unlock()
	}
	tok, err := src(ctx)
	if err != nil {
		return "", nil, Classify(c.route.Provider, c.route.Model, 401, "", err)
	}
	return base, map[string]string{"Authorization": "Bearer " + tok}, nil
}

// Request headers in the adapter order with route default headers last
func (c *geminiNativeClient) headers(auth map[string]string, accept string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	for k, v := range auth {
		h.Set(k, v)
	}
	h.Set("User-Agent", geminiAPIClient()+" (gemini-native)")
	h.Set("X-Goog-Api-Client", geminiAPIClient())
	for k, v := range c.route.Headers {
		h.Set(k, v)
	}
	if accept != "" {
		h.Set("Accept", accept)
	}
	return h
}

// One generateContent call prepared from the chat completions arguments
type geminiPrepared struct {
	request   map[string]any
	model     string
	url       string
	extra     map[string]any
	auth      map[string]string
	baseURL   string
	settings  wireSettings
	estTokens int
}

// Plain JSON form of an ordered body with numbers kept as written
func geminiPlain(o *omap) map[string]any {
	out := map[string]any{}
	if o == nil {
		return out
	}
	data, err := json.Marshal(o)
	if err != nil {
		return out
	}
	if parsed, err := geminiJSONLoads(string(data)); err == nil {
		if m, ok := parsed.(map[string]any); ok {
			return m
		}
	}
	return out
}

// hermes reasoning_config shape of a request reasoning ask, nil when unset
func geminiReasoningConfig(r Reasoning) map[string]any {
	rc := reasoningFromRequest(r)
	if !rc.set {
		return nil
	}
	return rc.object()
}

// Builds the generateContent body from the chat completions arguments the route would send
func (c *geminiNativeClient) prepare(ctx context.Context, req *Request) (*geminiPrepared, error) {
	settings := loadWireSettings(ctx)
	build := c.chat.cc.build(ctx, req, settings)
	c.chat.mu.Lock()
	c.chat.lastAliases = build.aliases
	c.chat.mu.Unlock()
	kwargs := geminiPlain(build.body)
	extra := geminiPlain(build.extraBody)
	if c.chat.cc.provider == "gemini" && isNativeGeminiBaseURL(c.route.BaseURL) && pyTruthy(extra["thinking_config"]) {
		if _, set := extra["max_output_tokens"]; !set {
			if budget := longDecodeMaxOutputTokens(req.Model, geminiReasoningConfig(req.Reasoning)); budget > 0 {
				extra["max_output_tokens"] = budget
			}
		}
	}
	maxTokens := kwargs["max_tokens"]
	if budget, ok := geminiIntValue(extra["max_output_tokens"]); ok {
		current, _ := geminiPyNumber(maxTokens)
		if budget > current {
			maxTokens = budget
		}
	}
	thinking := extra["thinking_config"]
	if !pyTruthy(thinking) {
		thinking = extra["thinkingConfig"]
	}
	responseFormat := kwargs["response_format"]
	if !pyTruthy(responseFormat) {
		responseFormat = extra["response_format"]
	}
	model := pyOrStr(kwargs["model"])
	if model == "" {
		model = req.Model
	}
	if model == "" {
		model = c.route.Model
	}
	baseURL, auth, err := c.credentials(ctx)
	if err != nil {
		return nil, err
	}
	messages, _ := kwargs["messages"].([]any)
	request := buildGeminiRequest(geminiRequestInput{Messages: messages, Tools: kwargs["tools"], ToolChoice: kwargs["tool_choice"],
		Temperature: kwargs["temperature"], MaxTokens: maxTokens, TopP: kwargs["top_p"], Stop: kwargs["stop"], ThinkingConfig: thinking,
		ResponseFormat: responseFormat, Model: model, ToolsAsJSONSchema: geminiAcceptsParametersJSONSchema(baseURL)})
	bare := bareGeminiModelID(model)
	return &geminiPrepared{request: request, model: bare, url: baseURL + "/models/" + bare + ":", extra: extra, auth: auth, baseURL: baseURL,
		settings: settings, estTokens: estimateBodyContextTokens(kwargs, DefaultImageTokenCost)}, nil
}

// Integer value of a JSON integer, false for floats, strings and booleans
func geminiIntValue(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
	}
	return 0, false
}

// Hop ceiling of a request, one past the hops its output budget can take
func geminiMaxHops(request map[string]any) int {
	gen, _ := request["generationConfig"].(map[string]any)
	return longDecodeHops(gen["maxOutputTokens"]) + 1
}

// Request copy carrying a continuation token
func geminiWithToken(request map[string]any, token string) map[string]any {
	out := copyAnyMap(request)
	out["continuationToken"] = token
	return out
}

// APIError for a Gemini failure, keeping the guidance message and the gemini error code
func (c *geminiNativeClient) apiError(gerr *GeminiAPIError) *APIError {
	apiErr := Classify(c.route.Provider, c.route.Model, gerr.StatusCode, gerr.Body, gerr)
	apiErr.Message = gerr.Message
	apiErr.Code = gerr.Code
	if gerr.RetryAfter > 0 {
		apiErr.RetryAfter = gerr.RetryAfter
	}
	if apiErr.RequestID == "" && gerr.Headers != nil {
		apiErr.RequestID = headerRequestID(gerr.Headers)
	}
	return apiErr
}

// Error for a non 200 response, without API key remedies on a bearer credential
func (c *geminiNativeClient) httpError(p *geminiPrepared, status int, headers http.Header, body string) *APIError {
	return c.apiError(geminiHTTPError(status, headers, sanitizeSurrogates(body), c.route.APIKey, p.baseURL, !c.bearer))
}

// One unary generateContent hop
func (c *geminiNativeClient) generate(ctx context.Context, p *geminiPrepared, request map[string]any, connect time.Duration) (map[string]any, int, http.Header, error) {
	body, err := geminiRequestJSON(request)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("provider: encode gemini request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url+geminiGenerateMethod, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	httpReq.Header = c.headers(p.auth, "")
	resp, err := c.chat.httpClient(connect).Do(httpReq)
	if err != nil {
		return nil, 0, nil, &geminiTransportErr{err: err, phase: TimeoutPhaseConnect}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, resp.Header.Clone(), &geminiTransportErr{err: err, phase: TimeoutPhaseRead}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, resp.Header.Clone(), c.httpError(p, resp.StatusCode, resp.Header.Clone(), string(data))
	}
	parsed, err := geminiJSONLoads(string(data))
	payload, ok := parsed.(map[string]any)
	if err != nil || !ok {
		if err == nil {
			err = errors.New("response body is not a JSON object")
		}
		gerr := &GeminiAPIError{Message: "Invalid JSON from Gemini native API: " + err.Error(), Code: "gemini_invalid_json", StatusCode: resp.StatusCode,
			Headers: resp.Header.Clone(), Body: string(data), Err: err}
		return nil, resp.StatusCode, resp.Header.Clone(), c.apiError(gerr)
	}
	return payload, resp.StatusCode, resp.Header.Clone(), nil
}

// Transport failure of one hop with the phase it happened in
type geminiTransportErr struct {
	err   error
	phase string
}

func (e *geminiTransportErr) Error() string { return e.err.Error() }

func (e *geminiTransportErr) Unwrap() error { return e.err }

// Complete runs unary generateContent hops until the long decode ends and returns the joined turn
func (c *geminiNativeClient) Complete(ctx context.Context, req *Request) (*Response, error) {
	p, err := c.prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	base, configured := c.chat.requestTimeout(req, p.settings)
	connect := 15 * time.Second
	if configured {
		connect = min(base, 60*time.Second)
	} else {
		base *= time.Duration(longDecodeStaleMultiplier(map[string]any{"extra_body": p.extra}))
	}
	start := time.Now()
	rctx, cancel := context.WithTimeout(ctx, base)
	defer cancel()
	fail := func(err error) (*Response, error) {
		var te *geminiTransportErr
		if !errors.As(err, &te) {
			return nil, err
		}
		inner := te.err
		if ctx.Err() == nil && errors.Is(rctx.Err(), context.DeadlineExceeded) {
			inner = &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: fmt.Errorf("Request timed out after %.0fs.", base.Seconds())}
		}
		return nil, c.chat.transportError(ctx, inner, te.phase)
	}
	first, status, headers, err := c.generate(rctx, p, p.request, connect)
	if err != nil {
		return fail(err)
	}
	hops := []map[string]any{first}
	maxHops := geminiMaxHops(p.request)
	for token := continuationToken(first); token != "" && len(hops) < maxHops; token = continuationToken(hops[len(hops)-1]) {
		hop, st, hdr, err := c.generate(rctx, p, geminiWithToken(p.request, token), connect)
		if err != nil {
			return fail(err)
		}
		hops, status, headers = append(hops, hop), st, hdr
	}
	merged := hops[0]
	if len(hops) > 1 {
		merged = mergeHopResponses(hops)
	}
	payload := translateGeminiResponse(merged, p.model)
	resp, err := c.chat.normalizeResponse(payload, status, headers, time.Since(start))
	if err != nil {
		return nil, err
	}
	usage, _ := payload["usage"].(map[string]any)
	markGeminiInitialUsage(resp, usage)
	return resp, nil
}

// Raw keys carrying the first hop prompt and cached tokens of a long decode turn
const (
	geminiRawInitialPrompt = "_initial_prompt_tokens"
	geminiRawInitialCached = "_initial_cached_tokens"
)

// Records the first hop prompt counts of a multi hop turn on the response
func markGeminiInitialUsage(resp *Response, usage map[string]any) {
	if resp == nil || usage == nil {
		return
	}
	prompt, ok := anyInt(usage["initial_prompt_tokens"])
	if !ok {
		return
	}
	cached, _ := anyInt(usage["initial_cached_tokens"])
	anthMarkRaw(resp, geminiRawInitialPrompt, prompt)
	anthMarkRaw(resp, geminiRawInitialCached, cached)
}

// ResponseInitialPromptTokens reports the first hop prompt and cached tokens of a long decode turn
func ResponseInitialPromptTokens(resp *Response) (int, int, bool) {
	prompt, ok := anyInt(rawResponseField(resp, geminiRawInitialPrompt))
	if !ok {
		return 0, 0, false
	}
	cached, _ := anyInt(rawResponseField(resp, geminiRawInitialCached))
	return prompt, cached, true
}

// CountTokens estimates the context of the chat completions arguments the request is built from
func (c *geminiNativeClient) CountTokens(ctx context.Context, req *Request) (int, error) {
	settings := loadWireSettings(ctx)
	build := c.chat.cc.build(ctx, req, settings)
	return estimateBodyContextTokens(geminiPlain(build.body), DefaultImageTokenCost), nil
}

// One message from the stream producer to the watchdog loop
type geminiStreamMsg struct {
	headers http.Header
	status  int
	chunks  []map[string]any
	err     error
	done    bool
}

// Stream runs streamGenerateContent hops, resuming dropped streams from their last checkpoint token
func (c *geminiNativeClient) Stream(ctx context.Context, req *Request, fn StreamFunc) (*Response, error) {
	p, err := c.prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	t := c.chat.streamTimeouts(req, p.settings, p.estTokens)
	if _, configured := c.chat.requestTimeout(req, p.settings); !configured {
		t.connect = 15 * time.Second
	}
	s := &ccStreamState{c: c.chat, req: req, fn: fn, estTokens: p.estTokens, tools: newCCToolCallAccumulator(), scrubber: NewStreamingThinkScrubber(),
		contentWatch: NewRunawayStreamWatch(), reasoningWatch: NewRunawayStreamWatch(), detailWatch: NewRunawayStreamWatch(),
		started: time.Now(), lifetimeCap: t.lifetime.Seconds()}
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	kill := &ccKill{}
	msgs := make(chan geminiStreamMsg, 32)
	go c.produceStream(attemptCtx, p, t.connect, msgs)
	resp, err := c.consumeGeminiStream(attemptCtx, cancel, kill, msgs, s, t)
	if err != nil {
		return nil, err
	}
	if u, ok := s.usage["initial_prompt_tokens"]; ok && u != nil {
		markGeminiInitialUsage(resp, s.usage)
	}
	return resp, nil
}

// Streams every hop of one turn, sending translated chunks per event and resuming drops from a committed token
func (c *geminiNativeClient) produceStream(ctx context.Context, p *geminiPrepared, connect time.Duration, out chan<- geminiStreamMsg) {
	send := func(m geminiStreamMsg) bool {
		select {
		case out <- m:
			return true
		case <-ctx.Done():
			return false
		}
	}
	slots := newGeminiToolSlots()
	var hopUsage []map[string]any
	maxHops := geminiMaxHops(p.request)
	body := p.request
	resume, sends := "", 0
	url := p.url + geminiStreamMethod + "?alt=sse"
	for {
		dropErr, fatal := c.streamRequest(ctx, p, url, body, connect, send, slots, &hopUsage, maxHops, &resume, &sends)
		if fatal != nil {
			send(geminiStreamMsg{err: fatal})
			return
		}
		if ctx.Err() != nil {
			return
		}
		if dropErr != nil {
			if resume == "" || sends >= geminiMaxTokenSends {
				gerr := &GeminiAPIError{Message: "Gemini streaming request failed: " + dropErr.err.Error(), Code: "gemini_stream_error",
					Err: ccWrapTransport(dropErr.err, dropErr.phase)}
				send(geminiStreamMsg{err: c.apiError(gerr)})
				return
			}
			catalogLog.Warn(fmt.Sprintf("Gemini stream dropped (%s); resuming from its last checkpoint", dropErr.err))
		}
		if resume == "" || sends >= geminiMaxTokenSends {
			send(geminiStreamMsg{done: true})
			return
		}
		body, sends = geminiWithToken(p.request, resume), sends+1
	}
}

// One streamGenerateContent request, a transport drop returned apart from fatal errors
func (c *geminiNativeClient) streamRequest(ctx context.Context, p *geminiPrepared, url string, request map[string]any, connect time.Duration,
	send func(geminiStreamMsg) bool, slots *geminiToolSlots, hopUsage *[]map[string]any, maxHops int, resume *string, sends *int) (*geminiTransportErr, error) {
	data, err := geminiRequestJSON(request)
	if err != nil {
		return nil, fmt.Errorf("provider: encode gemini request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header = c.headers(p.auth, "text/event-stream")
	resp, err := c.chat.httpClient(connect).Do(httpReq)
	if err != nil {
		return &geminiTransportErr{err: err, phase: TimeoutPhaseConnect}, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := readLimited(resp.Body, ccErrorBodyLimit)
		return nil, c.httpError(p, resp.StatusCode, resp.Header.Clone(), string(body))
	}
	if !send(geminiStreamMsg{headers: resp.Header.Clone(), status: resp.StatusCode}) {
		return nil, nil
	}
	reader := newGeminiSSEReader(resp.Body)
	for {
		event, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		if err != nil {
			return &geminiTransportErr{err: err, phase: TimeoutPhaseRead}, nil
		}
		hopsDone := len(*hopUsage)
		event, token := splitStreamContinuation(event, hopUsage)
		if token != "" && len(*hopUsage) > hopsDone && len(*hopUsage) >= maxHops {
			cand := copyAnyMap(geminiCandidate(event))
			cand["finishReason"] = "MAX_TOKENS"
			capped := copyAnyMap(event)
			capped["candidates"] = []any{cand}
			capped["usageMetadata"] = mergeHopUsage(*hopUsage)
			event, token = capped, ""
		}
		chunks := translateGeminiStreamEvent(event, p.model, slots)
		if token != "" || len(chunks) > 0 {
			*resume = token
			if token != "" {
				*sends = 0
			}
		}
		if !send(geminiStreamMsg{chunks: chunks}) {
			return nil, nil
		}
	}
}

// Drives the stale, read and lifetime watchdogs over the producer and feeds chunks to the chat stream state
func (c *geminiNativeClient) consumeGeminiStream(ctx context.Context, cancel context.CancelFunc, kill *ccKill, msgs <-chan geminiStreamMsg, s *ccStreamState, t ccTimeouts) (*Response, error) {
	staleTimer := time.NewTimer(t.stale)
	readTimer := time.NewTimer(t.read)
	defer staleTimer.Stop()
	defer readTimer.Stop()
	var lifetimeC <-chan time.Time
	if t.lifetime > 0 {
		lifetimeTimer := time.NewTimer(t.lifetime)
		defer lifetimeTimer.Stop()
		lifetimeC = lifetimeTimer.C
	}
	lastChunk := s.started
	model := s.req.Model
	if model == "" {
		model = "unknown"
	}
	for {
		select {
		case <-ctx.Done():
			return nil, c.chat.attemptError(ctx, kill, ctx.Err(), s)
		case <-staleTimer.C:
			elapsed := time.Since(lastChunk).Seconds()
			catalogLog.Warn(fmt.Sprintf("Stream stale for %.0fs (threshold %.0fs) — no chunks received. model=%s context=~%s tokens. Killing connection.", elapsed, t.stale.Seconds(), model, commaInt(s.estTokens)))
			_ = s.emit(StreamEvent{Kind: EventStatus, Text: fmt.Sprintf("⚠️ No response from provider for %ds (model: %s, context: ~%s tokens). Reconnecting...", int(elapsed), model, commaInt(s.estTokens))})
			kill.fire("stale", elapsed)
			cancel()
			return nil, c.chat.attemptError(ctx, kill, errors.New("stale stream"), s)
		case <-readTimer.C:
			kill.fire("read", t.read.Seconds())
			cancel()
			return nil, c.chat.attemptError(ctx, kill, errors.New("read timeout"), s)
		case <-lifetimeC:
			elapsed := time.Since(s.started).Seconds()
			catalogLog.Warn(fmt.Sprintf("Stream still running after %.0fs (stream_max_lifetime %.0fs) — killing connection. model=%s", elapsed, t.lifetime.Seconds(), model))
			_ = s.emit(StreamEvent{Kind: EventStatus, Text: fmt.Sprintf("⚠️ Response still streaming after %ds (stream_max_lifetime %ds, model: %s). Aborting...", int(elapsed), int(t.lifetime.Seconds()), model)})
			kill.fire("lifetime", elapsed)
			cancel()
			return nil, c.chat.attemptError(ctx, kill, errors.New("stream lifetime exceeded"), s)
		case m := <-msgs:
			if m.err != nil {
				return nil, m.err
			}
			if m.done {
				return s.finish(time.Since(s.started))
			}
			resetTimer(readTimer, t.read)
			if m.headers != nil {
				s.headers, s.status = m.headers, m.status
				continue
			}
			now := time.Now()
			lastChunk = now
			resetTimer(staleTimer, t.stale)
			if len(m.chunks) > 0 && s.firstChunkAt.IsZero() {
				s.firstChunkAt = now
			}
			for _, chunk := range m.chunks {
				stop, err := s.handleChunk(chunk)
				if err != nil {
					cancel()
					return nil, c.chat.chunkError(err)
				}
				if stop {
					cancel()
					return s.finish(time.Since(s.started))
				}
			}
		}
	}
}
