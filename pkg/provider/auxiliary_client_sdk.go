package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// AuxClient is one resolved auxiliary chat client, the OpenAI SDK client or a wire adapter
type AuxClient interface {
	BaseURL() string
	APIKey() string
	CreateChatCompletion(ctx context.Context, kwargs map[string]any) (any, error)
	Close() error
}

// OpenAI SDK defaults for timeout, connect timeout, retries and retry backoff in seconds
const (
	auxSDKDefaultTimeout        = 600.0
	auxSDKDefaultConnectTimeout = 5.0
	auxSDKDefaultMaxRetries     = 2
	auxSDKInitialRetryDelay     = 0.5
	auxSDKMaxRetryDelay         = 8.0
	auxSDKDefaultBaseURL        = "https://api.openai.com/v1"
	auxSDKUserAgent             = "icebath-agent"
)

// SDKDefaultTimeout is the OpenAI SDK default request timeout in seconds
const SDKDefaultTimeout = auxSDKDefaultTimeout

// Retry sleep seam so tests observe the SDK backoff without waiting
var auxSDKSleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shared default HTTP client of SDK clients built without a keepalive client
var auxSDKDefaultHTTPClient = sync.OnceValue(func() *http.Client {
	return &http.Client{Transport: auxTrackPoolConns(&http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: time.Duration(auxSDKDefaultConnectTimeout * float64(time.Second)), KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       PlatformTLSConfig(),
		TLSHandshakeTimeout:   time.Duration(auxSDKDefaultConnectTimeout * float64(time.Second)),
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		MaxConnsPerHost:       1000,
		IdleConnTimeout:       5 * time.Second,
		ExpectContinueTimeout: time.Second,
	})}
})

// SDKDefaultHTTPClient is the shared default HTTP client of SDK clients built without a keepalive client
func SDKDefaultHTTPClient() *http.Client { return auxSDKDefaultHTTPClient() }

// OpenAI SDK chat client of an auxiliary route
type auxOpenAIClient struct {
	auxEffectiveStamp
	mu             sync.Mutex
	apiKey         string
	apiKeyProvider func() (string, error)
	baseURL        string
	customHeaders  map[string]string
	customQuery    map[string]string
	timeout        float64
	hasTimeout     bool
	maxRetries     int
	httpClient     *http.Client
	ownsHTTP       bool
	closed         bool
	initErr        error
	responseHooks  []func(*http.Response)
	responseMarks  map[string]bool
}

// Appends a response hook to the SDK client once per mark, false when the client has no SDK HTTP client
func AddOpenAIClientResponseHook(client any, mark string, hook func(resp *http.Response)) bool {
	var c *auxOpenAIClient
	switch v := client.(type) {
	case *auxOpenAIClient:
		c = v
	case *auxAsyncOpenAIClient:
		if v != nil {
			c = v.auxOpenAIClient
		}
	}
	if c == nil || c.httpClient == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.responseMarks[mark] {
		return true
	}
	if c.responseMarks == nil {
		c.responseMarks = map[string]bool{}
	}
	c.responseMarks[mark] = true
	hooks := make([]func(*http.Response), 0, len(c.responseHooks)+1)
	c.responseHooks = append(append(hooks, c.responseHooks...), hook)
	return true
}

// Runs every registered response hook on each response the transport returns, redirect hops included
type auxResponseHookTransport struct {
	base  http.RoundTripper
	hooks []func(*http.Response)
}

func (t auxResponseHookTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	for _, h := range t.hooks {
		h(resp)
	}
	return resp, nil
}

// HTTP client of one request, the shared client unchanged unless response hooks are registered
func (c *auxOpenAIClient) requestHTTPClient() *http.Client {
	c.mu.Lock()
	hooks := c.responseHooks
	c.mu.Unlock()
	if len(hooks) == 0 {
		return c.httpClient
	}
	hc := *c.httpClient
	base := hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	hc.Transport = auxResponseHookTransport{base: base, hooks: hooks}
	return &hc
}

// Copy of the custom default headers a routed SDK client sends, nil when the client keeps none
func AuxClientCustomHeaders(c AuxClient) map[string]string {
	oc, ok := c.(*auxOpenAIClient)
	if !ok {
		return nil
	}
	oc.mu.Lock()
	defer oc.mu.Unlock()
	if len(oc.customHeaders) == 0 {
		return nil
	}
	return maps.Clone(oc.customHeaders)
}

// AuxClientDefaultHeaders is the header set an SDK client sends on every request, nil for adapters
func AuxClientDefaultHeaders(c AuxClient) map[string]string {
	oc, ok := c.(*auxOpenAIClient)
	if !ok || oc == nil {
		return nil
	}
	oc.mu.Lock()
	defer oc.mu.Unlock()
	out := map[string]string{"Accept": "application/json", "Content-Type": "application/json", "User-Agent": auxSDKUserAgent}
	if oc.apiKey != "" {
		out["Authorization"] = "Bearer " + oc.apiKey
	}
	if org := os.Getenv("OPENAI_ORG_ID"); org != "" {
		out["OpenAI-Organization"] = org
	}
	if project := os.Getenv("OPENAI_PROJECT_ID"); project != "" {
		out["OpenAI-Project"] = project
	}
	maps.Copy(out, oc.customHeaders)
	return out
}

// OpenAI SDK client with a static or callable key, the env key and base URL when none is given
func newAuxOpenAIClient(apiKey any, baseURL string, opts auxOpenAIOptions) *auxOpenAIClient {
	c := &auxOpenAIClient{customHeaders: map[string]string{}, customQuery: map[string]string{}, maxRetries: auxSDKDefaultMaxRetries}
	switch k := apiKey.(type) {
	case string:
		c.apiKey = k
	case func() (string, error):
		c.apiKeyProvider = k
	case func() string:
		c.apiKeyProvider = func() (string, error) { return k(), nil }
	case nil:
		env, ok := os.LookupEnv("OPENAI_API_KEY")
		if !ok {
			c.initErr = &AuxSDKError{Class: "OpenAIError", Message: "The api_key client option must be set either by passing api_key to the client or by setting the OPENAI_API_KEY environment variable"}
		}
		c.apiKey = env
	default:
		c.apiKey = fmt.Sprint(k)
	}
	if baseURL == "" {
		baseURL = os.Getenv("OPENAI_BASE_URL")
	}
	if baseURL == "" {
		baseURL = auxSDKDefaultBaseURL
	}
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}
	c.baseURL = baseURL
	for k, v := range opts.DefaultHeaders {
		c.customHeaders[k] = v
	}
	for k, v := range opts.DefaultQuery {
		c.customQuery[k] = v
	}
	if opts.MaxRetries != nil {
		c.maxRetries = *opts.MaxRetries
	}
	switch {
	case opts.Timeout > 0:
		c.timeout, c.hasTimeout = opts.Timeout, true
	case opts.HTTPClient != nil:
		c.hasTimeout = false
	default:
		c.timeout, c.hasTimeout = auxSDKDefaultTimeout, true
	}
	c.httpClient = opts.HTTPClient
	if c.httpClient == nil {
		c.httpClient = auxSDKDefaultHTTPClient()
	} else {
		c.ownsHTTP = true
	}
	return c
}

// NewOpenAIClient is the OpenAI SDK client with a static or callable key, the env key and base URL when none is given
func NewOpenAIClient(apiKey any, baseURL string, opts OpenAIClientOptions) AuxClient {
	return newAuxOpenAIClient(apiKey, baseURL, opts)
}

// Base URL with the trailing slash the SDK enforces
func (c *auxOpenAIClient) BaseURL() string { return c.baseURL }

// Static key, or the key the callable last minted
func (c *auxOpenAIClient) APIKey() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.apiKey
}

// Replaces the key the next request authenticates with, the SDK api_key attribute assignment
func (c *auxOpenAIClient) SetAPIKey(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.apiKey = key
}

var _ interface {
	APIKey() string
	SetAPIKey(string)
	IsClosed() bool
} = (*auxOpenAIClient)(nil)

// Closes the client's own HTTP client and marks the client closed, the shared SDK default client stays open
func (c *auxOpenAIClient) Close() error {
	if c.ownsHTTP {
		closeHTTPClient(c.httpClient)
	}
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

// Whether Close ran, the SDK is_closed of the underlying HTTP client
func (c *auxOpenAIClient) IsClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Mints a fresh key from the callable before every request
func (c *auxOpenAIClient) refreshAPIKey() (string, error) {
	c.mu.Lock()
	provider := c.apiKeyProvider
	c.mu.Unlock()
	if provider == nil {
		return c.APIKey(), nil
	}
	key, err := provider()
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.apiKey = key
	c.mu.Unlock()
	return key, nil
}

// One chat completions request split into body, headers, query and timeout
type auxSDKRequest struct {
	path    string
	body    []byte
	stream  bool
	headers map[string]any
	query   map[string]string
	timeout auxSDKTimeouts
}

// One httpx phase limit, a phase without a limit never times out
type auxPhaseLimit struct {
	d  time.Duration
	on bool
}

// Separate httpx pool, connect, write and read limits of one request
type auxSDKTimeouts struct{ pool, connect, write, read auxPhaseLimit }

// Same limit on every phase like an httpx float or None timeout
func auxUniformTimeouts(seconds float64, on bool) auxSDKTimeouts {
	l := auxPhaseLimit{d: secondsDuration(seconds), on: on}
	return auxSDKTimeouts{pool: l, connect: l, write: l, read: l}
}

// Phase limits of an httpx.Timeout value, a zero phase left without a limit
func auxPhaseTimeouts(t pyhttpx.Timeout) auxSDKTimeouts {
	limit := func(d time.Duration) auxPhaseLimit { return auxPhaseLimit{d: d, on: d > 0} }
	return auxSDKTimeouts{pool: limit(t.Pool), connect: limit(t.Connect), write: limit(t.Write), read: limit(t.Read)}
}

// Connect, read, write and pool budgets in seconds as the httpx.Timeout a streamed request carries
func sdkStreamTimeout(connect, read, write, pool float64) pyhttpx.Timeout {
	return pyhttpx.Timeout{Connect: secondsDuration(connect), Read: secondsDuration(read), Write: secondsDuration(write), Pool: secondsDuration(pool)}
}

func auxSDKBuildRequest(c *auxOpenAIClient, kwargs map[string]any) (*auxSDKRequest, error) {
	r := &auxSDKRequest{path: "chat/completions", query: map[string]string{}, headers: map[string]any{}, timeout: auxUniformTimeouts(c.timeout, c.hasTimeout)}
	body := map[string]any{}
	for k, v := range kwargs {
		switch k {
		case "extra_body", "extra_headers", "extra_query", "timeout":
		default:
			body[k] = v
		}
	}
	if extra, ok := kwargs["extra_body"].(map[string]any); ok {
		for k, v := range extra {
			body[k] = v
		}
	}
	switch h := kwargs["extra_headers"].(type) {
	case map[string]any:
		for k, v := range h {
			r.headers[k] = v
		}
	case map[string]string:
		for k, v := range h {
			r.headers[k] = v
		}
	}
	switch q := kwargs["extra_query"].(type) {
	case map[string]any:
		for k, v := range q {
			r.query[k] = pyrepr.Str(v)
		}
	case map[string]string:
		for k, v := range q {
			r.query[k] = v
		}
	}
	if t, ok := kwargs["timeout"]; ok {
		switch v := t.(type) {
		case nil:
			r.timeout = auxUniformTimeouts(0, false)
		case float64:
			r.timeout = auxUniformTimeouts(v, true)
		case float32:
			r.timeout = auxUniformTimeouts(float64(v), true)
		case int:
			r.timeout = auxUniformTimeouts(float64(v), true)
		case int64:
			r.timeout = auxUniformTimeouts(float64(v), true)
		case time.Duration:
			r.timeout = auxUniformTimeouts(v.Seconds(), true)
		case pyhttpx.Timeout:
			r.timeout = auxPhaseTimeouts(v)
		default:
			return nil, fmt.Errorf("Timeout value must be a float, a Timeout or None, got %s", pyrepr.Repr(v))
		}
	}
	r.stream, _ = body["stream"].(bool)
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	r.body = raw
	return r, nil
}

// Sends one chat completion with SDK retries, a response map or an SSE chunk stream when streaming
func (c *auxOpenAIClient) CreateChatCompletion(ctx context.Context, kwargs map[string]any) (any, error) {
	return c.create(ctx, "chat/completions", kwargs)
}

// Sends one Responses API request with SDK retries, a response map or an SSE event stream when streaming
func (c *auxOpenAIClient) CreateResponse(ctx context.Context, kwargs map[string]any) (any, error) {
	return c.create(ctx, "responses", kwargs)
}

// SDK create boundary seam so tests record the kwargs the typed request walk receives
var auxSDKBuildRequestFn = auxSDKBuildRequest

func (c *auxOpenAIClient) create(ctx context.Context, path string, kwargs map[string]any) (any, error) {
	out, _, err := c.createRaw(ctx, path, kwargs)
	return out, err
}

// Sends one chat completions request like with_raw_response, the parsed completion and the final response headers
func (c *auxOpenAIClient) CreateChatCompletionRaw(ctx context.Context, kwargs map[string]any) (any, http.Header, error) {
	return c.createRaw(ctx, "chat/completions", kwargs)
}

// SDK retry loop over the attempts, returning the last attempt's result with its response headers
func (c *auxOpenAIClient) createRaw(ctx context.Context, path string, kwargs map[string]any) (any, http.Header, error) {
	if c.initErr != nil {
		return nil, nil, c.initErr
	}
	req, err := auxSDKBuildRequestFn(c, kwargs)
	if err != nil {
		return nil, nil, err
	}
	req.path = path
	for retries := 0; ; retries++ {
		out, headers, retry, err := c.attempt(ctx, req, retries)
		if !retry || retries >= c.maxRetries {
			return out, headers, err
		}
		if err := auxSDKSleep(ctx, c.retryDelay(retries, headers)); err != nil {
			return nil, nil, err
		}
	}
}

// Backoff before the next retry, a short server retry-after wins over the jittered exponential delay
func (c *auxOpenAIClient) retryDelay(retries int, headers http.Header) time.Duration {
	if after, ok := auxSDKRetryAfter(headers); ok && after > 0 && after <= 60 {
		return time.Duration(after * float64(time.Second))
	}
	sleep := math.Min(auxSDKInitialRetryDelay*math.Pow(2, float64(min(retries, 1000))), auxSDKMaxRetryDelay)
	return time.Duration(math.Max(sleep*(1-0.25*rand.Float64()), 0) * float64(time.Second))
}

// Seconds from retry-after-ms, retry-after seconds or an HTTP date, false when absent or unparseable
func auxSDKRetryAfter(h http.Header) (float64, bool) {
	if h == nil {
		return 0, false
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(h.Get("retry-after-ms")), 64); err == nil {
		return v / 1000, true
	}
	raw := h.Get("retry-after")
	if v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil {
		return v, true
	}
	if t, err := mail.ParseDate(raw); err == nil {
		return time.Until(t).Seconds(), true
	}
	return 0, false
}

// Whether the SDK retries this status, x-should-retry overriding the status rule
func auxSDKShouldRetry(resp *http.Response) bool {
	switch resp.Header.Get("x-should-retry") {
	case "true":
		return true
	case "false":
		return false
	}
	return resp.StatusCode == 408 || resp.StatusCode == 409 || resp.StatusCode == 429 || resp.StatusCode >= 500
}

// One HTTP attempt returning the result, the retry headers, whether a retry is allowed and the error
func (c *auxOpenAIClient) attempt(ctx context.Context, r *auxSDKRequest, retries int) (any, http.Header, bool, error) {
	key, err := c.refreshAPIKey()
	if err != nil {
		return nil, nil, false, err
	}
	target, err := url.Parse(c.baseURL + r.path)
	if err != nil {
		return nil, nil, false, err
	}
	q := target.Query()
	for k, v := range c.customQuery {
		q.Set(k, v)
	}
	for k, v := range r.query {
		q.Set(k, v)
	}
	target.RawQuery = q.Encode()
	attemptCtx, cancel := context.WithCancel(ctx)
	stream := &auxSDKStream{cancel: cancel}
	idle := newAuxIdleTimer(r.timeout.pool, cancel)
	attemptCtx = httptrace.WithClientTrace(attemptCtx, &httptrace.ClientTrace{
		DNSStart:     func(httptrace.DNSStartInfo) { idle.arm(r.timeout.connect) },
		ConnectStart: func(string, string) { idle.arm(r.timeout.connect) },
		GotConn: func(info httptrace.GotConnInfo) {
			stream.mu.Lock()
			stream.conn = info.Conn
			stream.mu.Unlock()
			idle.arm(r.timeout.write)
		},
		WroteRequest: func(httptrace.WroteRequestInfo) { idle.arm(r.timeout.read) },
	})
	httpReq, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, target.String(), bytes.NewReader(r.body))
	if err != nil {
		idle.retire()
		cancel()
		return nil, nil, false, err
	}
	c.applyHeaders(httpReq, key, r, retries)
	resp, err := c.requestHTTPClient().Do(httpReq)
	idle.retire()
	if err != nil {
		cancel()
		if ctx.Err() != nil {
			return nil, nil, false, ctx.Err()
		}
		if idle.fired() {
			return nil, nil, true, auxSDKTimeoutError(&keepaliveTimeoutError{msg: "timed out"})
		}
		if auxIsTimeoutErr(err) {
			return nil, nil, true, auxSDKTimeoutError(err)
		}
		return nil, nil, true, &AuxSDKError{Class: "APIConnectionError", Message: "Connection error.", Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if retries < c.maxRetries && auxSDKShouldRetry(resp) {
			_ = resp.Body.Close()
			cancel()
			return nil, resp.Header, true, auxSDKStatusError(resp, nil)
		}
		data, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		if readErr != nil {
			return nil, nil, false, &AuxSDKError{Class: "APIConnectionError", Message: "Connection error.", Err: readErr}
		}
		return nil, nil, false, auxSDKStatusError(resp, data)
	}
	if r.stream {
		stream.resp = resp
		stream.idle = newAuxIdleTimer(r.timeout.read, func() { _ = resp.Body.Close() })
		stream.idle.stop()
		stream.lines = bufio.NewReaderSize(resp.Body, 64*1024)
		return stream, resp.Header, false, nil
	}
	body := &auxIdleReader{r: resp.Body, idle: newAuxIdleTimer(r.timeout.read, cancel)}
	data, err := io.ReadAll(body)
	body.idle.stop()
	_ = resp.Body.Close()
	cancel()
	if err != nil {
		if body.idle.fired() {
			return nil, nil, true, auxSDKTimeoutError(&keepaliveTimeoutError{msg: "timed out"})
		}
		if auxIsTimeoutErr(err) {
			return nil, nil, true, auxSDKTimeoutError(err)
		}
		return nil, nil, true, &AuxSDKError{Class: "APIConnectionError", Message: "Connection error.", Err: err}
	}
	var payload any
	if err := json.Unmarshal(data, &payload); err != nil {
		return string(data), resp.Header, false, nil
	}
	return payload, resp.Header, false, nil
}

// SDK headers, auth, organization and project, retry count, then client defaults and per request extras
func (c *auxOpenAIClient) applyHeaders(req *http.Request, key string, r *auxSDKRequest, retries int) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", auxSDKUserAgent)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if org := os.Getenv("OPENAI_ORG_ID"); org != "" {
		req.Header.Set("OpenAI-Organization", org)
	}
	if project := os.Getenv("OPENAI_PROJECT_ID"); project != "" {
		req.Header.Set("OpenAI-Project", project)
	}
	for k, v := range c.customHeaders {
		req.Header.Set(k, v)
	}
	for k, v := range r.headers {
		if v == nil {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, pyrepr.Str(v))
	}
	if req.Header.Get("X-Stainless-Retry-Count") == "" {
		req.Header.Set("X-Stainless-Retry-Count", strconv.Itoa(retries))
	}
}

// SDK status error with the Error code message, the error object body and the status class
func auxSDKStatusError(resp *http.Response, data []byte) *AuxSDKError {
	e := &AuxSDKError{StatusCode: resp.StatusCode, Headers: resp.Header.Clone(), Class: auxSDKStatusClass(resp.StatusCode)}
	if data == nil {
		e.Message = fmt.Sprintf("Error code: %d", resp.StatusCode)
		return e
	}
	text := strings.TrimSpace(pystr.DecodeUTF8Replace(data))
	var body any = text
	parsed, err := pyjson.Loads(text)
	if err == nil {
		body = parsed
		e.Message = fmt.Sprintf("Error code: %d - %s", resp.StatusCode, pyrepr.Str(parsed))
	} else if text != "" {
		e.Message = text
	} else {
		e.Message = fmt.Sprintf("Error code: %d", resp.StatusCode)
	}
	plain := auxSDKPlain(body)
	if m, ok := plain.(map[string]any); ok {
		if inner, has := m["error"]; has {
			plain = inner
		}
	}
	e.Body = plain
	if m, ok := plain.(map[string]any); ok {
		if v := m["code"]; v != nil {
			e.Code = pyrepr.Str(v)
		}
		if v := m["type"]; v != nil {
			e.Type = pyrepr.Str(v)
		}
	}
	return e
}

// Plain Go maps and slices for a pyjson value
func auxSDKPlain(v any) any {
	if _, ok := v.(string); ok {
		return v
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if json.Unmarshal(raw, &out) != nil {
		return v
	}
	return out
}

// SDK error class of an HTTP status
func auxSDKStatusClass(status int) string {
	switch {
	case status == 400:
		return "BadRequestError"
	case status == 401:
		return "AuthenticationError"
	case status == 403:
		return "PermissionDeniedError"
	case status == 404:
		return "NotFoundError"
	case status == 409:
		return "ConflictError"
	case status == 422:
		return "UnprocessableEntityError"
	case status == 429:
		return "RateLimitError"
	case status >= 500:
		return "InternalServerError"
	}
	return "APIStatusError"
}

// SDK APITimeoutError raised from a transport timeout
func auxSDKTimeoutError(cause error) *AuxSDKError {
	return &AuxSDKError{Class: "APITimeoutError", Message: "Request timed out.", Err: cause}
}

func auxIsTimeoutErr(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// Phase inactivity timer that runs its action once the phase limit passes without progress
type auxIdleTimer struct {
	mu      sync.Mutex
	t       *time.Timer
	limit   auxPhaseLimit
	action  func()
	retired bool
	didFire bool
}

func newAuxIdleTimer(limit auxPhaseLimit, action func()) *auxIdleTimer {
	it := &auxIdleTimer{action: action}
	it.arm(limit)
	return it
}

func (it *auxIdleTimer) fire() {
	it.mu.Lock()
	it.didFire = true
	it.mu.Unlock()
	it.action()
}

// Restarts the countdown under the limit of the phase now running, a phase without a limit stops it
func (it *auxIdleTimer) arm(limit auxPhaseLimit) {
	it.mu.Lock()
	defer it.mu.Unlock()
	if it.retired {
		return
	}
	it.limit = limit
	if !limit.on {
		if it.t != nil {
			it.t.Stop()
		}
		return
	}
	if it.t == nil {
		it.t = time.AfterFunc(limit.d, it.fire)
		return
	}
	it.t.Reset(limit.d)
}

func (it *auxIdleTimer) reset() {
	it.mu.Lock()
	defer it.mu.Unlock()
	if it.t != nil && it.limit.on {
		it.t.Reset(it.limit.d)
	}
}

func (it *auxIdleTimer) stop() {
	it.mu.Lock()
	defer it.mu.Unlock()
	if it.t != nil {
		it.t.Stop()
	}
}

// Stops the timer for good so a late phase callback never restarts it
func (it *auxIdleTimer) retire() {
	it.mu.Lock()
	defer it.mu.Unlock()
	it.retired = true
	if it.t != nil {
		it.t.Stop()
	}
}

func (it *auxIdleTimer) fired() bool {
	it.mu.Lock()
	defer it.mu.Unlock()
	return it.didFire
}

// Body reader that re-arms the read timeout after every successful read
type auxIdleReader struct {
	r    io.Reader
	idle *auxIdleTimer
}

func (r *auxIdleReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.idle.reset()
	}
	return n, err
}

// One server sent event as the SDK decoder emits it
type auxSSEEvent struct {
	event string
	data  string
}

// SDK chunk stream over an SSE body, io.EOF after [DONE] or the end of the body
type auxSDKStream struct {
	mu        sync.Mutex
	resp      *http.Response
	lines     *bufio.Reader
	idle      *auxIdleTimer
	cancel    context.CancelFunc
	conn      net.Conn
	released  bool
	onRelease func()
	eventName string
	hasEvent  bool
	data      []string
	hasID     bool
	retry     bool
	pending   []string
}

// Next SSE event per the SDK decoder, an unterminated trailing event is dropped
func (s *auxSDKStream) nextEvent() (*auxSSEEvent, error) {
	for {
		if len(s.pending) == 0 {
			chunk, err := s.readChunk()
			if err != nil {
				return nil, err
			}
			s.pending = auxSplitLines(chunk)
			continue
		}
		line := s.pending[0]
		s.pending = s.pending[1:]
		if ev := s.decode(line); ev != nil {
			return ev, nil
		}
	}
}

// Reads up to and including the next blank line boundary, the trailing partial chunk at EOF
func (s *auxSDKStream) readChunk() (string, error) {
	var b strings.Builder
	for {
		s.idle.reset()
		line, err := s.lines.ReadString('\n')
		s.idle.stop()
		b.WriteString(line)
		if err != nil {
			if b.Len() > 0 {
				return b.String(), nil
			}
			return "", err
		}
		if trimmed := strings.TrimRight(line, "\r\n"); trimmed == "" {
			return b.String(), nil
		}
	}
}

// Python str splitlines over one chunk, an empty string at each blank separator line
func auxSplitLines(chunk string) []string {
	var out []string
	for len(chunk) > 0 {
		i := strings.IndexAny(chunk, "\r\n")
		if i < 0 {
			out = append(out, chunk)
			break
		}
		out = append(out, chunk[:i])
		if chunk[i] == '\r' && i+1 < len(chunk) && chunk[i+1] == '\n' {
			chunk = chunk[i+2:]
		} else {
			chunk = chunk[i+1:]
		}
	}
	return out
}

// Feeds one line to the SDK decoder state, an event at each blank line that closes a non empty event
func (s *auxSDKStream) decode(line string) *auxSSEEvent {
	if line == "" {
		if !s.hasEvent && len(s.data) == 0 && !s.hasID && !s.retry {
			return nil
		}
		ev := &auxSSEEvent{event: s.eventName, data: strings.Join(s.data, "\n")}
		s.eventName, s.hasEvent, s.data, s.retry = "", false, nil, false
		return ev
	}
	if strings.HasPrefix(line, ":") {
		return nil
	}
	field, value, _ := strings.Cut(line, ":")
	value = strings.TrimPrefix(value, " ")
	switch field {
	case "event":
		s.eventName, s.hasEvent = value, true
	case "data":
		s.data = append(s.data, value)
	case "id":
		if !strings.Contains(value, "\x00") {
			s.hasID = true
		}
	case "retry":
		if _, err := strconv.Atoi(value); err == nil {
			s.retry = true
		}
	}
	return nil
}

// Next chunk map, io.EOF after [DONE] or the body end, a streamed error object raised as APIError
func (s *auxSDKStream) Next(ctx context.Context) (map[string]any, error) {
	s.mu.Lock()
	released := s.released
	s.mu.Unlock()
	if released {
		return nil, io.EOF
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			s.finish()
			return nil, err
		}
		stop := context.AfterFunc(ctx, func() { _ = s.resp.Body.Close() })
		defer stop()
	}
	for {
		ev, err := s.nextEvent()
		if err != nil {
			s.finish()
			if ctx != nil && ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			if s.idle.fired() {
				return nil, &AuxTransportError{Class: "ReadTimeout", Message: "timed out", Err: &keepaliveTimeoutError{msg: "timed out"}}
			}
			return nil, &AuxTransportError{Class: "RemoteProtocolError", Message: err.Error(), Err: err}
		}
		if strings.HasPrefix(ev.data, "[DONE]") {
			s.finish()
			return nil, io.EOF
		}
		var data any
		if err := json.Unmarshal([]byte(ev.data), &data); err != nil {
			s.finish()
			return nil, &AuxTransportError{Class: "JSONDecodeError", Message: err.Error(), Err: err, Doc: ev.data}
		}
		m, isMap := data.(map[string]any)
		if strings.HasPrefix(ev.event, "thread.") {
			return map[string]any{"data": data, "event": ev.event}, nil
		}
		if isMap && pyrepr.Truthy(m["error"]) {
			s.finish()
			return nil, auxSDKStreamErrorEvent(m["error"])
		}
		return m, nil
	}
}

// SDK APIError for an error object streamed in place of a chunk
func auxSDKStreamErrorEvent(errObj any) *AuxSDKError {
	e := &AuxSDKError{Class: "APIError", Body: errObj}
	if m, ok := errObj.(map[string]any); ok {
		if msg, ok := m["message"].(string); ok && msg != "" {
			e.Message = msg
		}
		if v := m["code"]; v != nil {
			e.Code = pyrepr.Str(v)
		}
		if v := m["type"]; v != nil {
			e.Type = pyrepr.Str(v)
		}
	}
	if e.Message == "" {
		e.Message = "An error occurred during streaming"
	}
	return e
}

// Closes the response once, running the pin release first so a late watchdog never cuts a pooled connection
func (s *auxSDKStream) finish() {
	s.mu.Lock()
	if s.released {
		s.mu.Unlock()
		return
	}
	s.released = true
	release := s.onRelease
	s.mu.Unlock()
	if release != nil {
		release()
	}
	s.idle.stop()
	_ = s.resp.Body.Close()
	s.cancel()
}

func (s *auxSDKStream) Close() error {
	s.finish()
	return nil
}

// Pins this attempt's connection and returns its shutdown, a no op once the stream has released it
func (s *auxSDKStream) auxPinAttempt(onRelease func()) func() {
	s.mu.Lock()
	s.onRelease = onRelease
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		conn, released := s.conn, s.released
		s.mu.Unlock()
		if !released && conn != nil {
			auxCutAttemptConn(conn)
			auxChatShutdownSocketFn(conn)
		}
	}
}

// Raw connection of the attempt while the stream still holds it, nil once released
func (s *auxSDKStream) auxAttemptConn() net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		return nil
	}
	return s.conn
}

// Shuts both directions of the raw TCP socket under any TLS or pool wrapper, closing other connections
func auxShutdownConn(conn net.Conn) {
	for {
		switch c := conn.(type) {
		case *tls.Conn:
			conn = c.NetConn()
			continue
		case interface{ NetConn() net.Conn }:
			conn = c.NetConn()
			continue
		case interface {
			CloseRead() error
			CloseWrite() error
		}:
			_ = c.CloseRead()
			_ = c.CloseWrite()
			return
		}
		_ = conn.Close()
		return
	}
}

// AuxTransportError is an httpx transport failure raised while a streamed response is consumed
type AuxTransportError struct {
	Class   string
	Message string
	Err     error
	Doc     string
	Request *FailedStreamRequest
}

func (e *AuxTransportError) Error() string      { return e.Message }
func (e *AuxTransportError) Unwrap() error      { return e.Err }
func (e *AuxTransportError) PyTypeName() string { return e.Class }

// Python module of the raised class, json.decoder for a JSONDecodeError and httpx for every transport class
func (e *AuxTransportError) PyModule() string {
	if e.Class == "JSONDecodeError" {
		return "json.decoder"
	}
	return "httpx"
}

func (e *AuxTransportError) FailedRequest() *FailedStreamRequest { return e.Request }
func (e *AuxTransportError) Timeout() bool                       { return strings.HasSuffix(e.Class, "Timeout") }

// Attempt socket shutdown seam the late watchdog timer test pauses
var auxChatShutdownSocketFn = auxShutdownConn

// Error every read of a cut attempt connection returns
var auxErrAttemptCut = errors.New("auxiliary attempt connection cut by its stream watchdog")

// Implemented by dialed connection wrappers that can refuse every later byte
type auxCuttableConn interface{ cutAttempt() }

// Cuts the wrapped attempt connection under any TLS layer so a delayed shutdown never reaches a pooled reuse
func auxCutAttemptConn(conn net.Conn) {
	for {
		switch c := conn.(type) {
		case auxCuttableConn:
			c.cutAttempt()
			return
		case *tls.Conn:
			conn = c.NetConn()
		case interface{ NetConn() net.Conn }:
			conn = c.NetConn()
		default:
			return
		}
	}
}
