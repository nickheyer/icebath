package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	openAIDefaultBaseURL    = "https://api.openai.com/v1"
	openAIDefaultMaxRetries = 2
	openAIDefaultTimeout    = 600 * time.Second
	openAIConnectTimeout    = 5 * time.Second
)

// Keyword parameters of images.generate and images.edit in the SDK body order
var (
	openAIGenerateParams = []string{"prompt", "background", "model", "moderation", "n", "output_compression", "output_format",
		"partial_images", "quality", "response_format", "size", "stream", "style", "user"}
	openAIEditParams = []string{"image", "prompt", "background", "input_fidelity", "mask", "model", "n", "output_compression",
		"output_format", "partial_images", "quality", "response_format", "size", "stream", "user"}
)

// Constructor arguments of openai.OpenAI, nil pointers mean the argument was not passed
type openAIClientOptions struct {
	APIKey         *string
	BaseURL        *string
	DefaultHeaders map[string]string
	HTTPClient     *http.Client
}

type openAIClient struct {
	APIKey, BaseURL        string
	Organization, Project  *string
	DefaultHeaders         map[string]string
	HTTPClient             *http.Client
	Timeout, ConnectTimout time.Duration
	MaxRetries             int
}

// Named in memory upload, the BytesIO with a name attribute images.edit expects
type openAIFile struct {
	Name string
	Data []byte
}

// Parsed images response, Object false when the body was not a JSON object
type openAIImagesResponse struct {
	Object bool
	Data   []any
	Usage  any
	Raw    any
}

// OpenAIError, APIConnectionError, APITimeoutError and the APIStatusError family of the SDK
type openAIError struct {
	Class      string
	Message    string
	StatusCode int
	Body       any
	Header     http.Header
	Err        error
}

func (e *openAIError) Error() string      { return e.Message }
func (e *openAIError) Unwrap() error      { return e.Err }
func (e *openAIError) PyTypeName() string { return e.Class }

// Client construction seam the image plugins call, tests swap it to observe the constructor arguments
var newOpenAIClientHook = newOpenAIClient

// Retry backoff sleep, tests replace it to skip the wait
var openAISleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Process environment lookup that tells unset apart from empty
func lookupProcessEnv(env *tools.Env, name string) (string, bool) {
	if env != nil && env.Secrets != nil {
		return env.Secrets.Get(name)
	}
	return os.LookupEnv(name)
}

// openai.OpenAI resolving the key, organization, project and base URL from the environment when not passed
func newOpenAIClient(env *tools.Env, o openAIClientOptions) (*openAIClient, error) {
	c := &openAIClient{DefaultHeaders: o.DefaultHeaders, HTTPClient: o.HTTPClient, Timeout: openAIDefaultTimeout,
		ConnectTimout: openAIConnectTimeout, MaxRetries: openAIDefaultMaxRetries}
	if o.APIKey != nil {
		c.APIKey = *o.APIKey
	} else if v, ok := lookupProcessEnv(env, "OPENAI_API_KEY"); ok {
		c.APIKey = v
	} else {
		return nil, &openAIError{Class: "OpenAIError", Message: "The api_key client option must be set either by passing api_key to the client or by setting the OPENAI_API_KEY environment variable"}
	}
	if v, ok := lookupProcessEnv(env, "OPENAI_ORG_ID"); ok {
		c.Organization = &v
	}
	if v, ok := lookupProcessEnv(env, "OPENAI_PROJECT_ID"); ok {
		c.Project = &v
	}
	switch {
	case o.BaseURL != nil:
		c.BaseURL = *o.BaseURL
	default:
		if v, ok := lookupProcessEnv(env, "OPENAI_BASE_URL"); ok {
			c.BaseURL = v
		} else {
			c.BaseURL = openAIDefaultBaseURL
		}
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true,
			MaxIdleConns: 1000, MaxIdleConnsPerHost: 1000, IdleConnTimeout: 5 * time.Second}}
	}
	return c, nil
}

// Request URL of an SDK path under the base URL with its enforced trailing slash
func (c *openAIClient) endpoint(path string) string {
	base := c.BaseURL
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + strings.TrimLeft(path, "/")
}

// Default, auth and custom headers the SDK sends on every request
func (c *openAIClient) headers(retries int) http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json")
	h.Set("User-Agent", UserAgent)
	h.Set("X-Stainless-Retry-Count", strconv.Itoa(retries))
	h.Set("X-Stainless-Read-Timeout", strconv.FormatFloat(c.Timeout.Seconds(), 'f', -1, 64))
	if c.APIKey != "" {
		h.Set("Authorization", "Bearer "+c.APIKey)
	}
	if c.Organization != nil {
		h.Set("OpenAI-Organization", *c.Organization)
	}
	if c.Project != nil {
		h.Set("OpenAI-Project", *c.Project)
	}
	for k, v := range c.DefaultHeaders {
		h.Set(k, v)
	}
	return h
}

// Ordered body of an images call, a TypeError for a keyword the SDK method does not accept
func openAIBody(method string, params []string, req map[string]any) ([]string, error) {
	known := map[string]bool{}
	for _, p := range params {
		known[p] = true
	}
	for k := range req {
		if !known[k] {
			return nil, &openAIError{Class: "TypeError", Message: fmt.Sprintf("Images.%s() got an unexpected keyword argument '%s'", method, k)}
		}
	}
	var keys []string
	for _, p := range params {
		if _, ok := req[p]; ok {
			keys = append(keys, p)
		}
	}
	return keys, nil
}

// images.generate posting the JSON body to /images/generations
func (c *openAIClient) ImagesGenerate(ctx context.Context, req map[string]any) (*openAIImagesResponse, error) {
	if _, ok := req["prompt"]; !ok {
		return nil, &openAIError{Class: "TypeError", Message: "Images.generate() missing 1 required keyword-only argument: 'prompt'"}
	}
	keys, err := openAIBody("generate", openAIGenerateParams, req)
	if err != nil {
		return nil, err
	}
	obj := pyjson.NewObject()
	for _, k := range keys {
		obj.Set(k, req[k])
	}
	body, err := pyjson.Dumps(obj, pyjson.AllowNaN(false))
	if err != nil {
		return nil, err
	}
	return c.post(ctx, "/images/generations", func() (io.Reader, string, error) {
		return strings.NewReader(body), "application/json", nil
	})
}

// images.edit posting a multipart form with image or image[] file parts to /images/edits
func (c *openAIClient) ImagesEdit(ctx context.Context, req map[string]any) (*openAIImagesResponse, error) {
	for _, required := range []string{"image", "prompt"} {
		if _, ok := req[required]; !ok {
			return nil, &openAIError{Class: "TypeError", Message: fmt.Sprintf("Images.edit() missing 1 required keyword-only argument: '%s'", required)}
		}
	}
	keys, err := openAIBody("edit", openAIEditParams, req)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	var files []struct {
		field string
		file  openAIFile
	}
	for _, k := range keys {
		switch v := req[k].(type) {
		case openAIFile:
			files = append(files, struct {
				field string
				file  openAIFile
			}{k, v})
		case []openAIFile:
			for _, f := range v {
				files = append(files, struct {
					field string
					file  openAIFile
				}{k + "[]", f})
			}
		default:
			if err := writeMultipartValue(w, k, v); err != nil {
				return nil, err
			}
		}
	}
	for _, f := range files {
		ctype, _ := pymimetypes.GuessFileType(f.file.Name, true)
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, f.field, f.file.Name))
		hdr.Set("Content-Type", ctype)
		part, err := w.CreatePart(hdr)
		if err != nil {
			return nil, err
		}
		part.Write(f.file.Data)
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	payload, ctype := buf.Bytes(), w.FormDataContentType()
	return c.post(ctx, "/images/edits", func() (io.Reader, string, error) { return bytes.NewReader(payload), ctype, nil })
}

// Form fields of one body value flattened with the SDK bracket array and nested key format
func writeMultipartValue(w *multipart.Writer, key string, v any) error {
	switch x := v.(type) {
	case nil:
		return nil
	case []any:
		for _, item := range x {
			if err := writeMultipartValue(w, key+"[]", item); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		for k, item := range x {
			if err := writeMultipartValue(w, key+"["+k+"]", item); err != nil {
				return err
			}
		}
		return nil
	case bool:
		return w.WriteField(key, strconv.FormatBool(x))
	}
	return w.WriteField(key, pyrepr.Str(v))
}

// Sends one images POST and parses its 2xx body as an ImagesResponse
func (c *openAIClient) post(ctx context.Context, path string, body func() (io.Reader, string, error)) (*openAIImagesResponse, error) {
	resp, raw, err := c.send(ctx, http.MethodPost, path, nil, body)
	if err != nil {
		return nil, err
	}
	return parseOpenAIImagesResponse(resp.Header, raw), nil
}

// Sends one SDK request with the default retries, timeouts and status error classes, returning the 2xx response
func (c *openAIClient) send(ctx context.Context, method, path string, header http.Header, body func() (io.Reader, string, error)) (*http.Response, []byte, error) {
	for retries := 0; ; retries++ {
		resp, raw, err := c.attempt(ctx, method, path, header, retries, body)
		remaining := c.MaxRetries - retries
		if err != nil {
			if remaining > 0 && ctx.Err() == nil {
				if serr := openAISleep(ctx, openAIRetryDelay(c.MaxRetries, remaining, nil)); serr == nil {
					continue
				}
			}
			return nil, nil, err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, raw, nil
		}
		if remaining > 0 && openAIShouldRetry(resp) {
			if serr := openAISleep(ctx, openAIRetryDelay(c.MaxRetries, remaining, resp.Header)); serr == nil {
				continue
			}
		}
		return nil, nil, openAIStatusError(resp, raw)
	}
}

// One HTTP attempt with the connect, write, read and per chunk timeouts of the SDK default Timeout
func (c *openAIClient) attempt(ctx context.Context, method, path string, header http.Header, retries int, body func() (io.Reader, string, error)) (*http.Response, []byte, error) {
	var reader io.Reader
	var ctype string
	if body != nil {
		var err error
		if reader, ctype, err = body(); err != nil {
			return nil, nil, err
		}
	}
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	timedOut := false
	timer := time.AfterFunc(c.Timeout, func() { mu.Lock(); timedOut = true; mu.Unlock(); cancel() })
	defer timer.Stop()
	trace := &httptrace.ClientTrace{
		ConnectStart:         func(string, string) { timer.Reset(c.ConnectTimout) },
		GotConn:              func(httptrace.GotConnInfo) { timer.Reset(c.Timeout) },
		WroteRequest:         func(httptrace.WroteRequestInfo) { timer.Reset(c.Timeout) },
		GotFirstResponseByte: func() { timer.Reset(c.Timeout) },
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(actx, trace), method, c.endpoint(path), reader)
	if err != nil {
		return nil, nil, &openAIError{Class: "APIConnectionError", Message: "Connection error.", Err: err}
	}
	req.Header = c.headers(retries)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	transportErr := func(err error) error {
		mu.Lock()
		defer mu.Unlock()
		if timedOut || errors.Is(err, context.DeadlineExceeded) {
			return &openAIError{Class: "APITimeoutError", Message: "Request timed out.", Err: err}
		}
		return &openAIError{Class: "APIConnectionError", Message: "Connection error.", Err: err}
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, nil, transportErr(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	chunk := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(chunk)
		buf.Write(chunk[:n])
		timer.Reset(c.Timeout)
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, nil, transportErr(rerr)
		}
	}
	return resp, buf.Bytes(), nil
}

// x-should-retry first, then 408, 409, 429 and every 5xx
func openAIShouldRetry(resp *http.Response) bool {
	switch resp.Header.Get("x-should-retry") {
	case "true":
		return true
	case "false":
		return false
	}
	s := resp.StatusCode
	return s == 408 || s == 409 || s == 429 || s >= 500
}

// Retry-After within a minute, else half a second doubling per retry capped at 8 s with up to 25 percent jitter
func openAIRetryDelay(maxRetries, remaining int, h http.Header) time.Duration {
	if h != nil {
		retryAfter := -1.0
		if ms, err := strconv.ParseFloat(h.Get("retry-after-ms"), 64); err == nil {
			retryAfter = ms / 1000
		} else if v := h.Get("retry-after"); v != "" {
			if s, err := strconv.ParseFloat(v, 64); err == nil {
				retryAfter = s
			} else if t, err := http.ParseTime(v); err == nil {
				retryAfter = time.Until(t).Seconds()
			}
		}
		if retryAfter > 0 && retryAfter <= 60 {
			return time.Duration(retryAfter * float64(time.Second))
		}
	}
	n := min(maxRetries-remaining, 1000)
	sleep := math.Min(0.5*math.Pow(2, float64(n)), 8.0)
	d := sleep * (1 - 0.25*rand.Float64())
	return time.Duration(math.Max(d, 0) * float64(time.Second))
}

var openAIStatusClasses = map[int]string{400: "BadRequestError", 401: "AuthenticationError", 403: "PermissionDeniedError",
	404: "NotFoundError", 409: "ConflictError", 422: "UnprocessableEntityError", 429: "RateLimitError"}

// APIStatusError subclass with the SDK message, Error code then the parsed body repr or the raw text
func openAIStatusError(resp *http.Response, raw []byte) error {
	text, _ := pyjson.DecodeBytes(raw)
	text = strings.TrimSpace(text)
	var body any = text
	msg := text
	if msg == "" {
		msg = fmt.Sprintf("Error code: %d", resp.StatusCode)
	}
	if parsed, err := pyjson.Loads(text); err == nil {
		body = parsed
		msg = fmt.Sprintf("Error code: %d - %s", resp.StatusCode, pyrepr.Repr(parsed))
	}
	if m, ok := pyjson.AsObject(body); ok {
		if inner, ok := m.Get("error"); ok {
			body = inner
		}
	}
	class := openAIStatusClasses[resp.StatusCode]
	if class == "" {
		class = "APIStatusError"
		if resp.StatusCode >= 500 {
			class = "InternalServerError"
		}
	}
	return &openAIError{Class: class, Message: msg, StatusCode: resp.StatusCode, Body: body, Header: resp.Header}
}

// ImagesResponse of a 2xx body, the raw text kept when the body is not JSON
func parseOpenAIImagesResponse(h http.Header, raw []byte) *openAIImagesResponse {
	text, _ := pyjson.DecodeBytes(raw)
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return &openAIImagesResponse{Raw: text}
	}
	out := &openAIImagesResponse{Raw: parsed}
	if m, ok := pyjson.Plain(parsed).(map[string]any); ok {
		out.Object = true
		out.Data, _ = m["data"].([]any)
		out.Usage = m["usage"]
	}
	return out
}

// Attribute of one response data item, nil when the item is not an object or lacks it
func openAIItemField(item any, key string) any {
	if m, ok := item.(map[string]any); ok {
		return m[key]
	}
	return nil
}

// String form of a response attribute for the cache writers, empty for None and falsy values
func openAIItemString(item any, key string) string {
	v := openAIItemField(item, key)
	if !pyTruthy(v) {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}

// Releases the pooled connections of the client transport
func (c *openAIClient) Close() {
	c.HTTPClient.CloseIdleConnections()
}
