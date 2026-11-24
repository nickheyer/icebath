package homeassistant

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/netpref"
	charsetutil "github.com/nickheyer/icebath/pkg/utils/charset"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	regexutil "github.com/nickheyer/icebath/pkg/utils/regex"
)

// Request or connection attempt that ran out of its time limit
type timeoutError struct {
	what  string
	limit time.Duration
}

func (e *timeoutError) Error() string { return e.what + " timed out after " + e.limit.String() }

// Always true: the error is a timeout
func (e *timeoutError) Timeout() bool { return true }

// The deadline the limit stood for, so errors.Is matches context.DeadlineExceeded
func (e *timeoutError) Unwrap() error { return context.DeadlineExceeded }

// HTTP error status of a REST response, or a body that is not JSON, with the URL that answered
type responseError struct {
	status  int
	message string
	url     string
}

func (e *responseError) Error() string {
	return fmt.Sprintf("HTTP %d %s from %s", e.status, e.message, e.url)
}

// Content types of a JSON body
var jsonContentTypeRE = regexutil.Lazy(regexp.MustCompile, `^application/(?:[\w.+-]+?\+)?json`)

// One fully read HTTP response
type httpResponse struct {
	status int
	reason string
	header http.Header
	body   []byte
	url    string
}

// Lowercased Content-Type header, empty when absent
func (r *httpResponse) contentType() string { return strings.ToLower(r.header.Get("Content-Type")) }

// Charset parameter of a Content-Type value, the first one with spaces and double quotes stripped
func mimetypeCharset(ctype string) string {
	if ctype == "" {
		return ""
	}
	parts := strings.Split(ctype, ";")
	for _, item := range parts[1:] {
		if item == "" {
			continue
		}
		key, value, _ := strings.Cut(item, "=")
		if strings.TrimSpace(strings.ToLower(key)) == "charset" {
			return strings.Trim(value, ` "`)
		}
	}
	return ""
}

// Canonical name of the charset the Content-Type declares, UTF-8 when it declares none or one that is unknown
func (r *httpResponse) encoding() string {
	if charset := mimetypeCharset(r.contentType()); charset != "" {
		if name, ok := charsetutil.Canonical(charset); ok {
			return name
		}
	}
	return "utf-8"
}

// Body as text in the charset the Content-Type names, undecodable bytes as U+FFFD
func (r *httpResponse) text() string { return httputil.DecodeText(r.header, r.body) }

// Body decoded as JSON in the declared charset, nil for a blank body, a responseError for a content type that is not JSON
func (r *httpResponse) json() (any, error) {
	ctype := r.contentType()
	if !jsonContentTypeRE().MatchString(ctype) {
		return nil, &responseError{status: r.status, message: fmt.Sprintf("response is not JSON (content type %q)", ctype), url: r.url}
	}
	stripped := bytes.Trim(r.body, " \t\n\r\x0b\x0c")
	if len(stripped) == 0 {
		return nil, nil
	}
	text, err := charsetutil.Decode(stripped, r.encoding())
	if err != nil {
		return nil, err
	}
	return jsonutil.Decode(text)
}

// HTTP/1.1 client asking for compressed bodies and decoding them, the proxy settings honoured only when trustEnv
func newSession(trustEnv bool) *http.Client {
	transport := netpref.Follow(http.DefaultTransport.(*http.Transport).Clone())
	transport.DialContext = netpref.DialContext((&net.Dialer{KeepAlive: 30 * time.Second}).DialContext)
	transport.TLSHandshakeTimeout = 0
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	transport.Proxy = nil
	if trustEnv {
		transport.Proxy = httputil.Proxy
	}
	return &http.Client{Transport: httputil.DecodingTransport(transport)}
}

// One request reading the whole body within the total time limit, a *timeoutError when the limit runs out
func doRequest(ctx context.Context, client *http.Client, method, rawURL string, headers map[string]string, body []byte,
	total time.Duration) (*httpResponse, error) {
	rctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(rctx, method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	var raw []byte
	if err == nil {
		raw, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			err = fmt.Errorf("reading the response of %s %s: %w", method, req.URL.Redacted(), err)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if rctx.Err() != nil {
			return nil, &timeoutError{what: method + " " + req.URL.Redacted(), limit: total}
		}
		return nil, err
	}
	reason := strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)+" ")
	return &httpResponse{status: resp.StatusCode, reason: reason, header: resp.Header, body: raw, url: resp.Request.URL.String()}, nil
}

// One JSON POST with the JSON content type
func postJSON(ctx context.Context, client *http.Client, rawURL string, headers map[string]string, payload any,
	total time.Duration) (*httpResponse, error) {
	text, err := jsonutil.Encode(payload, jsonutil.Spaced(), jsonutil.ASCII())
	if err != nil {
		return nil, err
	}
	merged := map[string]string{"Content-Type": "application/json"}
	for k, v := range headers {
		merged[k] = v
	}
	return doRequest(ctx, client, http.MethodPost, rawURL, merged, []byte(text), total)
}
