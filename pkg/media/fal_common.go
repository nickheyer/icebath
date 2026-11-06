package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const managedFalRateLimitRetryCapSeconds = 30.0

const falQueuePollSeconds = 2.0

const (
	falMaxAttempts = 10
	falBaseDelay   = 0.1
	falMaxDelay    = 30.0
	falUserAgent   = "icebath-fal"
)

var falRetryCodes = map[int]bool{408: true, 409: true, 429: true}

var falIngressCodes = map[int]bool{502: true, 503: true, 504: true}

// One HTTP response as the FAL client sees it
type falResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	URL        string
}

// Decoded JSON body of the response
func (r *falResponse) JSON() (any, error) {
	var v any
	if err := json.Unmarshal(r.Body, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// Error status raised by the FAL client for a non 2xx response
type falClientHTTPError struct {
	Message    string
	StatusCode int
	Response   *falResponse
}

func (e *falClientHTTPError) Error() string { return e.Message }

// Status of the attached response, the exception status otherwise
func (e *falClientHTTPError) ResponseStatus() (int, bool) {
	if e.Response == nil {
		return 0, false
	}
	return e.Response.StatusCode, true
}

// Status code carried on the exception itself
func (e *falClientHTTPError) ExcStatus() (int, bool) { return e.StatusCode, true }

// Missing FAL credentials for the direct queue
type falMissingCredentialsError struct{}

func (falMissingCredentialsError) Error() string {
	return "Please set the FAL_KEY environment variable to your API key."
}

// Request options one queue call carries
type falHTTPRequest struct {
	JSON    any
	Params  url.Values
	Timeout float64
	Headers map[string]string
}

// Internal client module helpers the managed adapter drives, any of them nil on an older SDK surface
type falClientInternals struct {
	MaybeRetryRequest func(ctx context.Context, c *http.Client, method, rawURL string, req falHTTPRequest) (*falResponse, error)
	RaiseForStatus    func(resp *falResponse) error
	SyncRequestHandle func(requestID, responseURL, statusURL, cancelURL string, c *http.Client) *falRequestHandle
	AddHintHeader     func(hint string, headers map[string]string)
	AddPriorityHeader func(priority string, headers map[string]string)
	AddTimeoutHeader  func(timeout float64, headers map[string]string)
	Sleep             func(seconds float64)
}

// Synchronous FAL client holding its key and authenticated HTTP client
type falSyncClient struct {
	Key            string
	DefaultTimeout *float64
	HTTP           *http.Client
}

// FAL client module surface the image and video tools load
type falClientModule struct {
	NewSyncClient func(key string) *falSyncClient
	Client        *falClientInternals
	Submit        func(ctx context.Context, env *tools.Env, application string, arguments map[string]any, headers map[string]string) (*falRequestHandle, error)
}

// Adds the key and user agent to every request like the SDK client default headers
type falAuthTransport struct {
	key  string
	base http.RoundTripper
}

func (t falAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if r.Header.Get("Authorization") == "" {
		r.Header.Set("Authorization", "Key "+t.key)
	}
	if r.Header.Get("User-Agent") == "" {
		r.Header.Set("User-Agent", falUserAgent)
	}
	return t.base.RoundTrip(r)
}

// Sync client whose HTTP client authenticates with the key
func newFalSyncClient(key string) *falSyncClient {
	timeout := 120.0
	return &falSyncClient{Key: key, DefaultTimeout: &timeout,
		HTTP: &http.Client{Transport: falAuthTransport{key: key, base: http.DefaultTransport}}}
}

// Exponential retry delay with jitter capped at the maximum
func falRetryDelay(numRetry int) float64 {
	delay := math.Min(falBaseDelay*math.Pow(2, float64(numRetry-1)), falMaxDelay)
	delay *= 0.5 + rand.Float64()
	return math.Min(delay, falMaxDelay)
}

// Reports whether a response is a retryable conflict, rate limit or non JSON ingress failure
func falShouldRetryResponse(resp *falResponse) bool {
	if falRetryCodes[resp.StatusCode] {
		return true
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	return falIngressCodes[resp.StatusCode] && !strings.Contains(ct, "application/json")
}

// Performs one request with its timeout and reads the full body
func falDo(ctx context.Context, c *http.Client, method, rawURL string, r falHTTPRequest) (*falResponse, error) {
	if len(r.Params) > 0 {
		sep := "?"
		if strings.Contains(rawURL, "?") {
			sep = "&"
		}
		rawURL += sep + r.Params.Encode()
	}
	var body io.Reader
	if r.JSON != nil {
		raw, err := json.Marshal(r.JSON)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(r.Timeout*float64(time.Second)))
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	if r.JSON != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &falResponse{StatusCode: resp.StatusCode, Header: resp.Header, Body: raw, URL: rawURL}, nil
}

// Default internals with the SDK retry policy on transport errors and retryable statuses
func newFalClientInternals() *falClientInternals {
	in := &falClientInternals{Sleep: func(s float64) { time.Sleep(time.Duration(s * float64(time.Second))) }}
	in.MaybeRetryRequest = func(ctx context.Context, c *http.Client, method, rawURL string, r falHTTPRequest) (*falResponse, error) {
		for numRetry := 0; ; {
			resp, err := falDo(ctx, c, method, rawURL, r)
			retry := (err != nil && ctx.Err() == nil) || (err == nil && falShouldRetryResponse(resp))
			if !retry || numRetry >= falMaxAttempts {
				return resp, err
			}
			numRetry++
			in.Sleep(falRetryDelay(numRetry))
		}
	}
	in.RaiseForStatus = falRaiseForStatus
	in.SyncRequestHandle = func(requestID, responseURL, statusURL, cancelURL string, c *http.Client) *falRequestHandle {
		return &falRequestHandle{RequestID: requestID, ResponseURL: responseURL, StatusURL: statusURL, CancelURL: cancelURL, Client: c, internals: in}
	}
	in.AddHintHeader = func(hint string, h map[string]string) { h["X-Fal-Runner-Hint"] = hint }
	in.AddPriorityHeader = func(priority string, h map[string]string) { h["X-Fal-Queue-Priority"] = priority }
	in.AddTimeoutHeader = func(timeout float64, h map[string]string) { h["X-Fal-Request-Timeout"] = pyrepr.Float(timeout) }
	return in
}

// Raises the client HTTP error carrying the JSON detail or the body text
func falRaiseForStatus(resp *falResponse) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	msg := pyhttpx.DecodeText(resp.Header, resp.Body)
	if v, err := resp.JSON(); err == nil {
		if m, ok := v.(map[string]any); ok {
			if d, ok := m["detail"]; ok {
				msg = pyrepr.Str(d)
			}
		}
	}
	return &falClientHTTPError{Message: msg, StatusCode: resp.StatusCode, Response: resp}
}

// Direct queue credentials from FAL_KEY or the FAL_KEY_ID and FAL_KEY_SECRET pair
func falFetchCredentials(env *tools.Env) (string, error) {
	if key := secret(env, "FAL_KEY"); key != "" {
		return key, nil
	}
	id, sec := secret(env, "FAL_KEY_ID"), secret(env, "FAL_KEY_SECRET")
	if id != "" && sec != "" {
		return id + ":" + sec, nil
	}
	return "", falMissingCredentialsError{}
}

// Queue origin honouring the FAL_RUN_HOST override
func falQueueURLFormat(env *tools.Env) string {
	host := secret(env, "FAL_RUN_HOST")
	if host == "" {
		host = "fal.run"
	}
	return "https://queue." + host + "/"
}

// Submits to the direct FAL queue and parses the request handle
func falSubmitRequest(ctx context.Context, in *falClientInternals, c *falSyncClient, rawURL string, arguments map[string]any, headers map[string]string) (*falRequestHandle, error) {
	timeout := 120.0
	if c.DefaultTimeout != nil {
		timeout = *c.DefaultTimeout
	}
	resp, err := in.MaybeRetryRequest(ctx, c.HTTP, http.MethodPost, rawURL, falHTTPRequest{JSON: arguments, Timeout: timeout, Headers: headers})
	if err != nil {
		return nil, err
	}
	if err := in.RaiseForStatus(resp); err != nil {
		return nil, err
	}
	return falHandleFromResponse(in, resp, c.HTTP)
}

// Request handle built from the queue submit JSON
func falHandleFromResponse(in *falClientInternals, resp *falResponse, c *http.Client) (*falRequestHandle, error) {
	v, err := resp.JSON()
	if err != nil {
		return nil, err
	}
	data, _ := v.(map[string]any)
	field := func(k string) (string, error) {
		s, ok := data[k].(string)
		if !ok {
			return "", fmt.Errorf("KeyError: %s", pyrepr.Repr(k))
		}
		return s, nil
	}
	var ids [4]string
	for i, k := range []string{"request_id", "response_url", "status_url", "cancel_url"} {
		if ids[i], err = field(k); err != nil {
			return nil, err
		}
	}
	return in.SyncRequestHandle(ids[0], ids[1], ids[2], ids[3], c), nil
}

// The compiled in FAL client module
func newFalClientModule() *falClientModule {
	mod := &falClientModule{NewSyncClient: newFalSyncClient, Client: newFalClientInternals()}
	mod.Submit = func(ctx context.Context, env *tools.Env, application string, arguments map[string]any, headers map[string]string) (*falRequestHandle, error) {
		key, err := falFetchCredentials(env)
		if err != nil {
			return nil, err
		}
		return falSubmitRequest(ctx, mod.Client, mod.NewSyncClient(key), falQueueURLFormat(env)+application, arguments, headers)
	}
	return mod
}

// Queue status event of one request
type falStatus struct {
	Kind          string
	QueuePosition any
	Logs          any
	Metrics       any
}

// In flight FAL queue request
type falRequestHandle struct {
	RequestID   string
	ResponseURL string
	StatusURL   string
	CancelURL   string
	Client      *http.Client
	internals   *falClientInternals
}

// Polls the status URL once and parses the queue state
func (h *falRequestHandle) Status(ctx context.Context, withLogs bool) (falStatus, error) {
	logs := "0"
	if withLogs {
		logs = "1"
	}
	resp, err := h.internals.MaybeRetryRequest(ctx, h.Client, http.MethodGet, h.StatusURL, falHTTPRequest{Params: url.Values{"logs": {logs}}})
	if err != nil {
		return falStatus{}, err
	}
	if err := h.internals.RaiseForStatus(resp); err != nil {
		return falStatus{}, err
	}
	v, err := resp.JSON()
	if err != nil {
		return falStatus{}, err
	}
	data, _ := v.(map[string]any)
	switch data["status"] {
	case "IN_QUEUE":
		return falStatus{Kind: "Queued", QueuePosition: data["queue_position"]}, nil
	case "IN_PROGRESS":
		return falStatus{Kind: "InProgress", Logs: data["logs"]}, nil
	case "COMPLETED":
		return falStatus{Kind: "Completed", Logs: data["logs"], Metrics: data["metrics"]}, nil
	}
	return falStatus{}, fmt.Errorf("Unknown status: %s", pyrepr.Str(data["status"]))
}

// Polls status at the interval until the request completes
func (h *falRequestHandle) IterEvents(ctx context.Context, withLogs bool, interval float64, each func(falStatus)) error {
	for {
		st, err := h.Status(ctx, withLogs)
		if err != nil {
			return err
		}
		if each != nil {
			each(st)
		}
		if st.Kind == "Completed" {
			return nil
		}
		h.internals.Sleep(interval)
	}
}

// Waits for completion at the SDK cadence and fetches the result JSON
func (h *falRequestHandle) Get(ctx context.Context) (any, error) {
	if err := h.IterEvents(ctx, false, 0.1, nil); err != nil {
		return nil, err
	}
	resp, err := h.internals.MaybeRetryRequest(ctx, h.Client, http.MethodGet, h.ResponseURL, falHTTPRequest{})
	if err != nil {
		return nil, err
	}
	if err := h.internals.RaiseForStatus(resp); err != nil {
		return nil, err
	}
	return resp.JSON()
}

// Drains queue events at the interval so the final get sees Completed on its first poll
func waitFalQueueResult(ctx context.Context, h *falRequestHandle, interval float64) (any, error) {
	if interval <= 0 {
		interval = falQueuePollSeconds
	}
	if err := h.IterEvents(ctx, false, interval, nil); err != nil {
		return nil, err
	}
	return h.Get(ctx)
}

var defaultFalClientModule = newFalClientModule()

// The FAL client module, compiled in so no runtime package acquisition is needed
func importFalClient() *falClientModule { return defaultFalClientModule }

// Trimmed queue origin with exactly one trailing slash
func normalizeFalQueueURLFormat(origin string) (string, error) {
	normalized := strings.TrimRight(strings.TrimSpace(origin), "/")
	if normalized == "" {
		return "", errors.New("Managed FAL queue origin is required")
	}
	return normalized + "/", nil
}

// HTTP status of a FAL or HTTP error, the response status first
func extractHTTPStatus(err error) (int, bool) {
	var rs interface{ ResponseStatus() (int, bool) }
	if errors.As(err, &rs) {
		if s, ok := rs.ResponseStatus(); ok {
			return s, true
		}
	}
	var es interface{ ExcStatus() (int, bool) }
	if errors.As(err, &es) {
		return es.ExcStatus()
	}
	return 0, false
}

// Response body JSON of an error carrying an HTTP response
func errorResponseJSON(err error) (map[string]any, bool) {
	var fe *falClientHTTPError
	if !errors.As(err, &fe) || fe.Response == nil {
		return nil, false
	}
	v, jerr := fe.Response.JSON()
	if jerr != nil {
		return nil, false
	}
	m, ok := v.(map[string]any)
	return m, ok
}

// Readable tail for a managed gateway BILLING_ERROR response, empty otherwise
func managedFalBillingError(err error, what string) string {
	payload, ok := errorResponseJSON(err)
	if !ok {
		return ""
	}
	e, _ := payload["error"].(map[string]any)
	if e == nil || e["code"] != "BILLING_ERROR" {
		return ""
	}
	details, _ := e["details"].(map[string]any)
	upstream, _ := details["upstreamPayload"].(map[string]any)
	code := firstTruthy(upstream["code"], details["chargeIntentErrorCode"], "billing_error")
	detail := firstTruthy(upstream["error"], "Nous Portal rejected the charge authorization")
	message := firstTruthy(e["message"], "Charge authorization failed")
	return fmt.Sprintf("%s (BILLING_ERROR; %s: %s). ", message, code, detail) +
		"This is a Nous Portal billing configuration issue, not a missing local API key. " +
		fmt.Sprintf("The managed route cannot run this %s until Nous enables its billing meter; ", what) +
		"a direct FAL_KEY is an optional bypass."
}

// Python or chain over values rendered with str
func firstTruthy(values ...any) string {
	for _, v := range values {
		if pyrepr.Truthy(v) {
			return pyrepr.Str(v)
		}
	}
	return pyrepr.Str(values[len(values)-1])
}

// Seconds a 429 asked to wait from Retry-After then the body retryAfter
func managedFalRetryAfterSeconds(err error) (float64, bool) {
	var fe *falClientHTTPError
	if !errors.As(err, &fe) || fe.Response == nil {
		return 0, false
	}
	if s, ok := extractHTTPStatus(err); !ok || s != 429 {
		return 0, false
	}
	var raw any
	if vals, ok := fe.Response.Header["Retry-After"]; ok && len(vals) > 0 {
		raw = vals[0]
	} else {
		payload, ok := errorResponseJSON(err)
		if !ok {
			return 0, false
		}
		e, _ := payload["error"].(map[string]any)
		raw = e["retryAfter"]
	}
	return pyFloat(raw)
}

// Python float() of a string or number, false where it would raise
func pyFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		f, err := pyint.ParseFloat(x)
		return f, err == nil
	}
	return 0, false
}

// Message naming a managed gateway rate limit and its retry hint
func managedFalRateLimitMessage(what, name string, retryAfter float64, hasRetry bool) string {
	hint := "no Retry-After given"
	if hasRetry {
		g, _ := pyrepr.Format(retryAfter, "g")
		hint = "retry after " + g + "s"
	}
	return fmt.Sprintf("Nous Subscription gateway rate-limited %s '%s' (HTTP 429; %s). ", what, name, hint) +
		"The model is enabled — retry later instead of switching models or setting FAL_KEY."
}

var (
	falMonotonic = time.Now
	falWaitSleep = time.Sleep
)

// Wrapped rate limit error that keeps the 429 cause
type falRateLimitError struct {
	msg   string
	cause error
}

func (e *falRateLimitError) Error() string { return e.msg }
func (e *falRateLimitError) Unwrap() error { return e.cause }

// Submits with a fresh idempotency key, waiting out one capped 429 Retry-After before one resubmit
func submitManagedFalWithRateLimitRetry(ctx context.Context, submit func(headers map[string]string) (*falRequestHandle, error), what, name string) (*falRequestHandle, error) {
	for attempt := 1; attempt <= 2; attempt++ {
		handle, err := submit(map[string]string{"x-idempotency-key": uuid4()})
		if err == nil {
			return handle, nil
		}
		if s, ok := extractHTTPStatus(err); !ok || s != 429 {
			return nil, err
		}
		retryAfter, has := managedFalRetryAfterSeconds(err)
		if attempt == 2 || !has || retryAfter > managedFalRateLimitRetryCapSeconds {
			return nil, &falRateLimitError{msg: managedFalRateLimitMessage(what, name, retryAfter, has), cause: err}
		}
		deadline := falMonotonic().Add(time.Duration(retryAfter * float64(time.Second)))
		for falMonotonic().Before(deadline) {
			if tools.IsInterrupted(ctx) {
				return nil, &falRateLimitError{msg: managedFalRateLimitMessage(what, name, retryAfter, has), cause: err}
			}
			falWaitSleep(min(500*time.Millisecond, max(0, deadline.Sub(falMonotonic()))))
		}
	}
	return nil, nil
}

// Missing SDK surface error for managed gateway mode
func falRequire(ok bool, what string) error {
	if !ok {
		return fmt.Errorf("%s is required for managed FAL gateway mode", what)
	}
	return nil
}

// Drives a Nous managed fal queue gateway through the FAL client primitives
type managedFalSyncClient struct {
	queueURLFormat string
	syncClient     *falSyncClient
	httpClient     *http.Client
	internals      *falClientInternals
}

// Managed client over the given module surface, refusing an incomplete surface up front
func newManagedFalSyncClient(mod *falClientModule, key, queueRunOrigin string) (*managedFalSyncClient, error) {
	if err := falRequire(mod != nil && mod.NewSyncClient != nil, "fal_client.SyncClient"); err != nil {
		return nil, err
	}
	if err := falRequire(mod.Client != nil, "fal_client.client"); err != nil {
		return nil, err
	}
	format, err := normalizeFalQueueURLFormat(queueRunOrigin)
	if err != nil {
		return nil, err
	}
	sc := mod.NewSyncClient(key)
	if err := falRequire(sc != nil && sc.HTTP != nil, "fal_client.SyncClient._client"); err != nil {
		return nil, err
	}
	in := mod.Client
	if in.MaybeRetryRequest == nil || in.RaiseForStatus == nil {
		return nil, errors.New("fal_client.client request helpers are required for managed FAL gateway mode")
	}
	if err := falRequire(in.SyncRequestHandle != nil, "fal_client.client.SyncRequestHandle"); err != nil {
		return nil, err
	}
	return &managedFalSyncClient{queueURLFormat: format, syncClient: sc, httpClient: sc.HTTP, internals: in}, nil
}

// Optional submit arguments of the managed client
type falSubmitOptions struct {
	Path         string
	Hint         *string
	WebhookURL   *string
	Priority     *string
	Headers      map[string]string
	StartTimeout *float64
}

// Posts to the gateway queue, retrying only requests without an idempotency key
func (c *managedFalSyncClient) Submit(ctx context.Context, application string, arguments map[string]any, o falSubmitOptions) (*falRequestHandle, error) {
	rawURL := c.queueURLFormat + application
	if o.Path != "" {
		rawURL += "/" + strings.TrimLeft(o.Path, "/")
	}
	if o.WebhookURL != nil {
		rawURL += "?" + url.Values{"fal_webhook": {*o.WebhookURL}}.Encode()
	}
	headers := map[string]string{}
	for k, v := range o.Headers {
		headers[k] = v
	}
	if o.Hint != nil && c.internals.AddHintHeader != nil {
		c.internals.AddHintHeader(*o.Hint, headers)
	}
	if o.Priority != nil {
		if c.internals.AddPriorityHeader == nil {
			return nil, errors.New("fal_client.client.add_priority_header is required for priority requests")
		}
		c.internals.AddPriorityHeader(*o.Priority, headers)
	}
	if o.StartTimeout != nil {
		if c.internals.AddTimeoutHeader == nil {
			return nil, errors.New("fal_client.client.add_timeout_header is required for timeout requests")
		}
		c.internals.AddTimeoutHeader(*o.StartTimeout, headers)
	}
	timeout := 120.0
	if c.syncClient != nil && c.syncClient.DefaultTimeout != nil {
		timeout = *c.syncClient.DefaultTimeout
	}
	req := falHTTPRequest{JSON: arguments, Timeout: timeout, Headers: headers}
	keyed := false
	for k := range headers {
		if strings.EqualFold(k, "x-idempotency-key") {
			keyed = true
		}
	}
	var resp *falResponse
	var err error
	if keyed {
		resp, err = falDo(ctx, c.httpClient, http.MethodPost, rawURL, req)
	} else {
		resp, err = c.internals.MaybeRetryRequest(ctx, c.httpClient, http.MethodPost, rawURL, req)
	}
	if err != nil {
		return nil, err
	}
	if err := c.internals.RaiseForStatus(resp); err != nil {
		return nil, err
	}
	return falHandleFromResponse(c.internals, resp, c.httpClient)
}
