package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
)

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

// Internal client module helpers the queue submit and poll paths drive
type falClientInternals struct {
	MaybeRetryRequest func(ctx context.Context, c *http.Client, method, rawURL string, req falHTTPRequest) (*falResponse, error)
	RaiseForStatus    func(resp *falResponse) error
	SyncRequestHandle func(requestID, responseURL, statusURL, cancelURL string, c *http.Client) *falRequestHandle
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
	return in
}

// Raises the client HTTP error carrying the JSON detail or the body text
func falRaiseForStatus(resp *falResponse) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	msg := httputil.DecodeText(resp.Header, resp.Body)
	if v, err := resp.JSON(); err == nil {
		if m, ok := v.(map[string]any); ok {
			if d, ok := m["detail"]; ok {
				msg = anyutil.String(d)
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
			return "", fmt.Errorf("the fal.ai queue reply has no %s", k)
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
	return falStatus{}, fmt.Errorf("Unknown status: %s", anyutil.String(data["status"]))
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
