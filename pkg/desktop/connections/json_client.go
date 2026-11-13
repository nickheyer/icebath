package connections

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// DefaultFetchTimeoutMs is the socket idle budget of a REST call without its own timeout
const DefaultFetchTimeoutMs = 30_000

// Upload is one file a REST call sends as multipart form data
type Upload struct {
	Filename    string
	ContentType string
	Bytes       []byte
}

// JSONOptions shape one JSON REST call, HasBody set when Body is sent even as null
type JSONOptions struct {
	Method      string
	Body        any
	HasBody     bool
	Headers     map[string]string
	TimeoutMs   float64
	Bearer      string
	Upload      *Upload
	Gateway     backend.Connection
	ReplayOn401 bool
}

// Milliseconds of a positive finite timeout rounded, the fallback otherwise
func resolveTimeoutMs(timeoutMs, fallbackMs float64) int {
	fallback := float64(DefaultFetchTimeoutMs)
	if !math.IsNaN(fallbackMs) && !math.IsInf(fallbackMs, 0) && fallbackMs > 0 {
		fallback = math.Round(fallbackMs)
	}
	if !math.IsNaN(timeoutMs) && !math.IsInf(timeoutMs, 0) && timeoutMs > 0 {
		return int(math.Round(timeoutMs))
	}
	return int(fallback)
}

// Options with extra headers under the caller's own, the caller winning on a shared name
func requestOptionsWithHeaders(o JSONOptions, headers map[string]string) JSONOptions {
	merged := make(map[string]string, len(headers)+len(o.Headers))
	for k, v := range headers {
		merged[k] = v
	}
	for k, v := range o.Headers {
		merged[k] = v
	}
	o.Headers = merged
	return o
}

var unsafeFilename = regexp.MustCompile("[\"\r\n]")

// Multipart form body with one file field and its content type header value
func multipartBody(upload Upload) ([]byte, string) {
	raw := make([]byte, 12)
	_, _ = rand.Read(raw)
	boundary := "----hermes-" + hex.EncodeToString(raw)
	filename := upload.Filename
	if filename == "" {
		filename = "file"
	}
	filename = unsafeFilename.ReplaceAllString(filename, "_")
	contentType := upload.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	var b bytes.Buffer
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString(`Content-Disposition: form-data; name="file"; filename="` + filename + "\"\r\n")
	b.WriteString("Content-Type: " + contentType + "\r\n\r\n")
	b.Write(upload.Bytes)
	b.WriteString("\r\n--" + boundary + "--\r\n")
	return b.Bytes(), "multipart/form-data; boundary=" + boundary
}

// JSON text of a request body the way JSON.stringify writes it
func jsonBody(body any) ([]byte, error) {
	text, err := jsonMarshal(body)
	if err != nil {
		return nil, err
	}
	return []byte(text), nil
}

var htmlStart = regexp.MustCompile(`(?i)^\s*<(?:!doctype|html)`)

// First n UTF-16 units of text, the way String.prototype.slice counts
func jsSlicePrefix(text string, n int) string {
	units := utf16.Encode([]rune(text))
	if len(units) <= n {
		return text
	}
	return string(utf16.Decode(units[:n]))
}

// Parsed JSON body of a finished exchange, nil for an empty body, the html and invalid JSON errors otherwise
func decodeJSONText(rawURL string, status int, text, contentType string, htmlErr func() error) (any, error) {
	if text == "" {
		return nil, nil
	}
	if htmlStart.MatchString(text) || strings.Contains(contentType, "text/html") {
		return nil, htmlErr()
	}
	var out any
	if json.Unmarshal([]byte(text), &out) != nil {
		return nil, fmt.Errorf("Invalid JSON from %s (status %d): %s", rawURL, status, jsSlicePrefix(text, 200))
	}
	return out, nil
}

// Status text of a response without its code, as Node exposes statusMessage
func statusMessage(resp *http.Response) string {
	if _, rest, ok := strings.Cut(resp.Status, " "); ok {
		return rest
	}
	return resp.Status
}

// idleTimer cancels an exchange after a stretch of socket inactivity, as Node req.setTimeout does
type idleTimer struct {
	timer    *time.Timer
	budget   time.Duration
	timedOut atomic.Bool
}

func newIdleTimer(ms int, cancel func()) *idleTimer {
	t := &idleTimer{budget: time.Duration(ms) * time.Millisecond}
	t.timer = time.AfterFunc(t.budget, func() {
		t.timedOut.Store(true)
		cancel()
	})
	return t
}

func (t *idleTimer) touch() { t.timer.Reset(t.budget) }
func (t *idleTimer) stop()  { t.timer.Stop() }

// Context whose transport progress keeps the idle timer alive
func (t *idleTimer) trace(ctx context.Context) context.Context {
	touch := func() { t.touch() }
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn:              func(httptrace.GotConnInfo) { touch() },
		ConnectDone:          func(string, string, error) { touch() },
		DNSDone:              func(httptrace.DNSDoneInfo) { touch() },
		WroteHeaders:         touch,
		WroteRequest:         func(httptrace.WroteRequestInfo) { touch() },
		GotFirstResponseByte: touch,
	})
}

type touchReader struct {
	r io.Reader
	t *idleTimer
}

func (r touchReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.t.touch()
	}
	return n, err
}

var noRedirectClient = &http.Client{
	Transport:     jsonTransport,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// One Node http.request exchange that never follows redirects and reads the whole JSON answer
func nodeJSONExchange(ctx context.Context, rawURL, method string, headers http.Header, body []byte, timeoutMs int, state *RequestState) (any, error) {
	parsed, err := parseURL(rawURL)
	if err != nil {
		return nil, errors.New("Invalid URL")
	}
	if parsed.Protocol() != "http:" && parsed.Protocol() != "https:" {
		return nil, errors.New("Unsupported Icebath backend URL protocol: " + parsed.Protocol())
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := newIdleTimer(timeoutMs, cancel)
	defer idle.stop()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(idle.trace(ctx), method, parsed.Href(false), reader)
	if err != nil {
		return nil, err
	}
	req.Header = headers
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	timedOut := func() error {
		return fmt.Errorf("Timed out connecting to Icebath backend after %dms", timeoutMs)
	}
	state.BodySent = true
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		if idle.timedOut.Load() {
			return nil, timedOut()
		}
		return nil, nodeTransportError(err, parsed.Host())
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(touchReader{resp.Body, idle})
	if err != nil {
		if idle.timedOut.Load() {
			return nil, timedOut()
		}
		return nil, nodeTransportError(err, parsed.Host())
	}
	text := strings.ToValidUTF8(string(raw), "�")
	status := resp.StatusCode
	if status >= 400 {
		return nil, HTTPStatusError(status, text, statusMessage(resp))
	}
	if status >= 300 {
		return nil, HTMLResponseError(rawURL, status, resp.Header.Get("Location"))
	}
	return decodeJSONText(rawURL, status, text, resp.Header.Get("Content-Type"), func() error {
		return HTMLResponseError(rawURL, status, "")
	})
}

// Header set in insertion order of the spreads, later names replacing earlier ones case insensitively
func spreadHeaders(layers ...map[string]string) http.Header {
	out := http.Header{}
	for _, layer := range layers {
		for name, value := range layer {
			out.Set(name, value)
		}
	}
	return out
}

// JSON REST call with the session token header, a bearer, or the native gateway grant headers
func (s *Service) fetchJSON(ctx context.Context, rawURL string, token *string, o JSONOptions) (any, error) {
	method := orGet(o.Method)
	return WithRetry(func(state *RequestState) (any, error) {
		var native map[string]string
		if o.Gateway != nil {
			endpoint := o.Gateway.Endpoint()
			if endpoint == nil {
				return nil, errors.New("Native gateway descriptor has no endpoint")
			}
			var grantBody any
			if o.Upload == nil && o.HasBody {
				grantBody = o.Body
			}
			headers, err := backend.NativeGatewayHTTPHeaders(*endpoint, jsString(o.Gateway["baseUrl"]), rawURL, s.icebathHome, method, grantBody)
			if err != nil {
				return nil, err
			}
			native = headers
		}
		var body []byte
		contentType := "application/json"
		if o.Upload != nil {
			body, contentType = multipartBody(*o.Upload)
		} else if o.HasBody {
			encoded, err := jsonBody(o.Body)
			if err != nil {
				return nil, err
			}
			body = encoded
		}
		auth := native
		if auth == nil {
			value := "null"
			if token != nil {
				value = *token
			}
			auth = map[string]string{"X-Icebath-Session-Token": value}
		}
		bearer := map[string]string{}
		if o.Bearer != "" {
			bearer["Authorization"] = "Bearer " + o.Bearer
		}
		headers := spreadHeaders(s.headersForRemoteRequest(rawURL), o.Headers, map[string]string{"Content-Type": contentType}, auth, bearer)
		return nodeJSONExchange(ctx, rawURL, method, headers, body, resolveTimeoutMs(o.TimeoutMs, DefaultFetchTimeoutMs), state)
	}, DefaultRetryPolicy(method))
}

// Credential free JSON call for public gateway endpoints, never sending a session token
func (s *Service) fetchPublicJSON(ctx context.Context, rawURL string, o JSONOptions) (any, error) {
	method := orGet(o.Method)
	return WithRetry(func(state *RequestState) (any, error) {
		var body []byte
		if o.HasBody {
			encoded, err := jsonBody(o.Body)
			if err != nil {
				return nil, err
			}
			body = encoded
		}
		if _, err := parseURL(rawURL); err != nil {
			return nil, errors.New("Invalid URL: Invalid URL")
		}
		headers := spreadHeaders(s.headersForRemoteRequest(rawURL), o.Headers, map[string]string{"Content-Type": "application/json"})
		return nodeJSONExchange(ctx, rawURL, method, headers, body, resolveTimeoutMs(o.TimeoutMs, DefaultFetchTimeoutMs), state)
	}, DefaultRetryPolicy(method))
}

// Cookieless JSON POST for the native token and refresh exchanges
func (s *Service) postJSONNoAuth(ctx context.Context, rawURL string, body any, o JSONOptions) (any, error) {
	o.Method = "POST"
	o.Body = body
	o.HasBody = true
	return s.fetchJSON(ctx, rawURL, nil, o)
}
