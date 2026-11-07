package messagetools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const DefaultGraphBaseURL = "https://graph.microsoft.com/v1.0"

// Graph client failure, the base class of MicrosoftGraphAPIError
type MicrosoftGraphClientError struct {
	Msg string
	Err error
}

func (e *MicrosoftGraphClientError) Error() string { return e.Msg }
func (e *MicrosoftGraphClientError) Unwrap() error { return e.Err }

// Failed Graph API request with its status, request line, Retry-After and decoded body
type MicrosoftGraphAPIError struct {
	StatusCode        int
	Method, URL       string
	Message           string
	RetryAfterSeconds *float64
	Payload           any
}

// API error carrying the detail message for its formatted text
func NewMicrosoftGraphAPIError(statusCode int, method, url, message string, retryAfterSeconds *float64, payload any) *MicrosoftGraphAPIError {
	return &MicrosoftGraphAPIError{StatusCode: statusCode, Method: method, URL: url, Message: message,
		RetryAfterSeconds: retryAfterSeconds, Payload: payload}
}

func (e *MicrosoftGraphAPIError) Error() string {
	return fmt.Sprintf("Microsoft Graph API error %d for %s %s: %s", e.StatusCode, e.Method, e.URL, e.Message)
}

// Matches a MicrosoftGraphClientError target the way except catches a subclass
func (e *MicrosoftGraphAPIError) As(target any) bool {
	if t, ok := target.(**MicrosoftGraphClientError); ok {
		*t = &MicrosoftGraphClientError{Msg: e.Error()}
		return true
	}
	return false
}

// Token provider the client authenticates with, MicrosoftGraphTokenProvider or a static token source
type GraphTokenSource interface {
	GetAccessToken(ctx context.Context, forceRefresh bool) (string, error)
	ClearCache()
}

// Client settings, nil fields taking the v1.0 base URL, sixty second timeout, three retries and default user agent
type MicrosoftGraphClientOptions struct {
	BaseURL    *string
	Timeout    *time.Duration
	MaxRetries *int
	Transport  http.RoundTripper
	Sleep      func(ctx context.Context, seconds float64) error
	UserAgent  *string
}

// Async Graph REST client retrying transport errors, 401 with a token refetch, and 429 or 5xx after Retry-After
type MicrosoftGraphClient struct {
	TokenProvider GraphTokenSource
	BaseURL       string
	Timeout       time.Duration
	MaxRetries    int
	UserAgent     string
	transport     http.RoundTripper
	sleep         func(ctx context.Context, seconds float64) error
}

// Client authenticated by the app only token provider
func NewMicrosoftGraphClient(tp *MicrosoftGraphTokenProvider, opts ...MicrosoftGraphClientOptions) *MicrosoftGraphClient {
	return NewMicrosoftGraphClientFor(tp, opts...)
}

// Client authenticated by any token source with the option overrides applied
func NewMicrosoftGraphClientFor(source GraphTokenSource, opts ...MicrosoftGraphClientOptions) *MicrosoftGraphClient {
	c := &MicrosoftGraphClient{TokenProvider: source, BaseURL: DefaultGraphBaseURL, Timeout: 60 * time.Second,
		MaxRetries: 3, UserAgent: "Icebath-Agent/graph-client", sleep: contextSleep}
	for _, o := range opts {
		if o.BaseURL != nil {
			c.BaseURL = *o.BaseURL
		}
		if o.Timeout != nil {
			c.Timeout = *o.Timeout
		}
		if o.MaxRetries != nil {
			c.MaxRetries = max(0, *o.MaxRetries)
		}
		if o.Transport != nil {
			c.transport = o.Transport
		}
		if o.Sleep != nil {
			c.sleep = o.Sleep
		}
		if o.UserAgent != nil {
			c.UserAgent = *o.UserAgent
		}
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	return c
}

// Sleeps the seconds unless the context ends first like asyncio.sleep under cancellation
func contextSleep(ctx context.Context, seconds float64) error {
	t := time.NewTimer(time.Duration(seconds * float64(time.Second)))
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Decoded body of a GET
func (c *MicrosoftGraphClient) GetJSON(ctx context.Context, path string, params map[string]any, headers map[string]string) (any, error) {
	resp, err := c.request(ctx, http.MethodGet, path, params, nil, headers)
	if err != nil {
		return nil, err
	}
	return decodeGraphJSON(resp)
}

// Decoded body of a POST carrying the JSON body
func (c *MicrosoftGraphClient) PostJSON(ctx context.Context, path string, jsonBody any, headers map[string]string) (any, error) {
	resp, err := c.request(ctx, http.MethodPost, path, nil, jsonBody, headers)
	if err != nil {
		return nil, err
	}
	return decodeGraphJSON(resp)
}

// Decoded body of a PATCH, an empty object for a 204 or bodiless response
func (c *MicrosoftGraphClient) PatchJSON(ctx context.Context, path string, jsonBody any, headers map[string]string) (any, error) {
	resp, err := c.request(ctx, http.MethodPatch, path, nil, jsonBody, headers)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusNoContent && len(resp.Content) > 0 {
		return decodeGraphJSON(resp)
	}
	return pyjson.NewObject(), nil
}

// Decoded body of a DELETE, or deleted true with the status code for a 204 or bodiless response
func (c *MicrosoftGraphClient) DeleteValue(ctx context.Context, path string, headers map[string]string) (any, error) {
	resp, err := c.request(ctx, http.MethodDelete, path, nil, nil, headers)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusNoContent && len(resp.Content) > 0 {
		return decodeGraphJSON(resp)
	}
	out := pyjson.NewObject()
	out.Set("deleted", true)
	out.Set("status_code", int64(resp.StatusCode))
	return out, nil
}

// DELETE result as the dict the method declares, a non object body being a client error
func (c *MicrosoftGraphClient) Delete(ctx context.Context, path string, headers map[string]string) (map[string]any, error) {
	v, err := c.DeleteValue(ctx, path, headers)
	if err != nil {
		return nil, err
	}
	switch d := v.(type) {
	case *pyjson.Object:
		return d.Vals, nil
	case map[string]any:
		return d, nil
	}
	return nil, &MicrosoftGraphClientError{Msg: fmt.Sprintf("Expected Graph delete response dict, got %s.", pyrepr.TypeName(v))}
}

// Every page value list concatenated while following the odata nextLink
func (c *MicrosoftGraphClient) CollectPaginated(ctx context.Context, path string, params map[string]any, headers map[string]string) ([]any, error) {
	items := []any{}
	nextURL, nextParams := c.resolveURL(path), params
	for nextURL != "" {
		var ps map[string]any
		if len(nextParams) > 0 {
			ps = nextParams
		}
		resp, err := c.request(ctx, http.MethodGet, nextURL, ps, nil, headers)
		if err != nil {
			return nil, err
		}
		payload, err := decodeGraphJSON(resp)
		if err != nil {
			return nil, err
		}
		get, ok := jsonDict(payload)
		if !ok {
			return nil, &MicrosoftGraphClientError{Msg: fmt.Sprintf("Expected paginated Graph response dict, got %s.", pyrepr.TypeName(payload))}
		}
		if list, ok := get("value").([]any); ok {
			items = append(items, list...)
		}
		next := get("@odata.nextLink")
		if !pyrepr.Truthy(next) {
			break
		}
		s, ok := next.(string)
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute 'startswith'", pyrepr.TypeName(next))
		}
		nextURL, nextParams = s, nil
	}
	return items, nil
}

// Streams a Graph resource to destination through a part file renamed into place on success
func (c *MicrosoftGraphClient) DownloadToFile(ctx context.Context, path, destination string, headers map[string]string) (map[string]any, error) {
	return c.DownloadToFileChunked(ctx, path, destination, headers, 65536)
}

// Download reading the body chunkSize bytes at a time
func (c *MicrosoftGraphClient) DownloadToFileChunked(ctx context.Context, path, destination string, headers map[string]string, chunkSize int) (map[string]any, error) {
	rawURL, target := c.resolveURL(path), pypath.PathStr(destination)
	if err := os.MkdirAll(pypath.PathParent(target), 0o777); err != nil {
		return nil, err
	}
	name := pypath.PathName(target)
	if name == "" {
		flavour := "PosixPath"
		if runtime.GOOS == "windows" {
			flavour = "WindowsPath"
		}
		return nil, fmt.Errorf("%s(%s) has an empty name", flavour, pyrepr.StrRepr(target))
	}
	tmpTarget := pypath.PathJoin(pypath.PathParent(target), name+".part")
	unlinkPart := func() {
		if ctx.Err() == nil {
			os.Remove(tmpTarget)
		}
	}
	perform := func(client *http.Client, hdr http.Header) (*pyhttpx.Response, any, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, nil, err
		}
		req.Header = hdr
		s, err := pyhttpx.Stream(client, req, c.Timeout)
		if err != nil {
			unlinkPart()
			return nil, nil, transportFailure{err}
		}
		defer s.Body.Close()
		if s.StatusCode >= 400 {
			body, err := io.ReadAll(s.Body)
			if err != nil {
				unlinkPart()
				return nil, nil, transportFailure{err}
			}
			s.Content = body
			return &s.Response, nil, nil
		}
		f, err := os.OpenFile(tmpTarget, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
		if err != nil {
			return nil, nil, err
		}
		buf := make([]byte, max(1, chunkSize))
		for {
			n, rerr := io.ReadFull(s.Body, buf)
			if n > 0 {
				if _, werr := f.Write(buf[:n]); werr != nil {
					f.Close()
					return nil, nil, werr
				}
			}
			if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
				break
			}
			if rerr != nil {
				f.Close()
				unlinkPart()
				return nil, nil, transportFailure{rerr}
			}
		}
		if err := f.Close(); err != nil {
			return nil, nil, err
		}
		var contentType any
		if vals := s.Header.Values("Content-Type"); len(vals) > 0 {
			contentType = strings.Join(vals, ", ")
		}
		return &s.Response, contentType, nil
	}
	contentType, err := c.withRetries(ctx, http.MethodGet, rawURL, "*/*", false, headers, perform, "download")
	if err != nil {
		return nil, err
	}
	if err := os.Rename(tmpTarget, target); err != nil {
		return nil, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, err
	}
	return map[string]any{"path": target, "size_bytes": info.Size(), "content_type": contentType}, nil
}

// httpx.HTTPError raised inside an attempt, the only failure the retry loop absorbs
type transportFailure struct{ err error }

func (t transportFailure) Error() string { return t.err.Error() }
func (t transportFailure) Unwrap() error { return t.err }

// httpx primitive_value_to_str of one query value, lists expanding into repeated entries
func queryValues(v any) []string {
	switch x := v.(type) {
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			out = append(out, queryValues(item)...)
		}
		return out
	case []string:
		return x
	case bool:
		if x {
			return []string{"true"}
		}
		return []string{"false"}
	case nil:
		return []string{""}
	}
	return []string{pyrepr.Str(v)}
}

// Response of a JSON request with the method and URL it was sent with
type graphResponse struct {
	*pyhttpx.Response
	method, url string
}

// Sends one JSON request under the retry policy with params merged into the URL
func (c *MicrosoftGraphClient) request(ctx context.Context, method, pathOrURL string, params map[string]any, jsonBody any, headers map[string]string) (*graphResponse, error) {
	rawURL := c.resolveURL(pathOrURL)
	reqURL := rawURL
	if params != nil {
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		var ps pyhttpx.Params
		for _, k := range keys {
			for _, v := range queryValues(params[k]) {
				ps = append(ps, [2]string{k, v})
			}
		}
		reqURL = pyhttpx.MergeParams(rawURL, ps)
	}
	var body []byte
	if jsonBody != nil {
		text, err := pyjson.Dumps(jsonBody, pyjson.EnsureASCII(false), pyjson.Separators(",", ":"), pyjson.AllowNaN(false))
		if err != nil {
			return nil, err
		}
		body = []byte(text)
	}
	perform := func(client *http.Client, hdr http.Header) (*pyhttpx.Response, any, error) {
		var rd io.Reader
		if body != nil {
			rd = strings.NewReader(string(body))
		}
		req, err := http.NewRequestWithContext(ctx, method, reqURL, rd)
		if err != nil {
			return nil, nil, err
		}
		req.Header = hdr
		resp, err := pyhttpx.Do(client, req, c.Timeout)
		if err != nil {
			return nil, nil, transportFailure{err}
		}
		return resp, resp, nil
	}
	got, err := c.withRetries(ctx, method, rawURL, "application/json", jsonBody != nil, headers, perform, "request")
	if err != nil {
		return nil, err
	}
	return &graphResponse{Response: got.(*pyhttpx.Response), method: method, url: reqURL}, nil
}

// Request headers with bearer token, accept and user agent, caller headers replacing equal keys
func (c *MicrosoftGraphClient) requestHeaders(token, accept string, hasJSON bool, headers map[string]string) http.Header {
	pairs := [][2]string{{"Authorization", "Bearer " + token}, {"Accept", accept}, {"User-Agent", c.UserAgent}}
	if hasJSON {
		pairs = append(pairs, [2]string{"Content-Type", "application/json"})
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		i := slices.IndexFunc(pairs, func(p [2]string) bool { return p[0] == k })
		if i >= 0 {
			pairs[i][1] = headers[k]
		} else {
			pairs = append(pairs, [2]string{k, headers[k]})
		}
	}
	h := http.Header{}
	for _, p := range pairs {
		h.Add(p[0], p[1])
	}
	return h
}

// Runs perform under the retry policy, refreshing the token only after a 401 and raising once retries run out
func (c *MicrosoftGraphClient) withRetries(ctx context.Context, method, rawURL, accept string, hasJSON bool, headers map[string]string,
	perform func(client *http.Client, hdr http.Header) (*pyhttpx.Response, any, error), kind string) (any, error) {
	var lastErr error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		apiErr, isAPI := lastErr.(*MicrosoftGraphAPIError)
		token, err := c.TokenProvider.GetAccessToken(ctx, isAPI && apiErr.StatusCode == 401)
		if err != nil {
			return nil, err
		}
		exhausted := attempt >= c.MaxRetries
		client, closeClient := freshHTTPClient(c.transport)
		resp, result, err := perform(client, c.requestHeaders(token, accept, hasJSON, headers))
		closeClient()
		var retryResp *pyhttpx.Response
		if err != nil {
			var tf transportFailure
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if !errors.As(err, &tf) {
				return nil, err
			}
			lastErr = tf.err
			if exhausted {
				return nil, &MicrosoftGraphClientError{Msg: fmt.Sprintf("Microsoft Graph %s failed for %s %s: %s", kind, method, rawURL, pyhttpx.ErrorText(tf.err)), Err: tf.err}
			}
		} else {
			if resp.StatusCode < 400 {
				return result, nil
			}
			built := buildGraphAPIError(method, rawURL, resp)
			lastErr = built
			status := resp.StatusCode
			if exhausted || !(status == 401 || status == 429 || (status >= 500 && status < 600)) {
				return nil, built
			}
			if status == 401 {
				c.TokenProvider.ClearCache()
			}
			retryResp = resp
		}
		if err := c.sleep(ctx, graphRetryDelay(retryResp, attempt)); err != nil {
			return nil, err
		}
	}
	return nil, &MicrosoftGraphClientError{Msg: fmt.Sprintf("Microsoft Graph %s exhausted retries for %s %s.", kind, method, rawURL)}
}

// Absolute URL kept as is, a path joined under the base URL
func (c *MicrosoftGraphClient) resolveURL(pathOrURL string) string {
	if strings.HasPrefix(pathOrURL, "http://") || strings.HasPrefix(pathOrURL, "https://") {
		return pathOrURL
	}
	if strings.HasPrefix(pathOrURL, "/") {
		return c.BaseURL + pathOrURL
	}
	return c.BaseURL + "/" + pathOrURL
}

// Body parsed as JSON, a client error naming the request when it is not valid JSON
func decodeGraphJSON(resp *graphResponse) (any, error) {
	v, err := resp.JSON()
	if err != nil {
		return nil, &MicrosoftGraphClientError{Msg: fmt.Sprintf("Microsoft Graph response was not valid JSON for %s %s", resp.method, resp.url), Err: err}
	}
	return v, nil
}

// Retry-After seconds when the response names them, else half a second doubling per attempt up to eight
func graphRetryDelay(resp *pyhttpx.Response, attempt int) float64 {
	if resp != nil {
		if v, ok := pyhttpx.RetryAfterSeconds(resp.Header); ok {
			return v
		}
	}
	return min(8.0, 0.5*float64(int64(1)<<min(attempt, 62)))
}

// API error from a failed response, the Graph error detail preferred over the raw text
func buildGraphAPIError(method, rawURL string, resp *pyhttpx.Response) *MicrosoftGraphAPIError {
	payload, err := resp.JSON()
	if err != nil {
		payload = nil
	}
	detail, ok := "", false
	if get, isDict := jsonDict(payload); isDict {
		detail, ok = FormatGraphError(get("error"))
	}
	if !ok {
		detail = pystr.Strip(resp.Text())
		if detail == "" {
			detail = "unknown error"
		}
	}
	var retryAfter *float64
	if v, has := pyhttpx.RetryAfterSeconds(resp.Header); has {
		retryAfter = &v
	}
	return NewMicrosoftGraphAPIError(resp.StatusCode, method, rawURL, detail, retryAfter, payload)
}
