package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	DefaultURL  = "https://config-config.nousresearch.com"
	URLEnv      = "ICEBATH_CONFIG_REMOTE_URL"
	InstanceEnv = "ICEBATH_CONFIG_INSTANCE_ID"
	httpTimeout = 15 * time.Second
)

// One plane answer, HasRetryAfter set when a Retry-After header parsed as a number
type Response struct {
	Status        int
	Body          map[string]any
	ETag          string
	RetryAfter    float64
	HasRetryAfter bool
}

// Error code of the body, http_STATUS when the body names none
func (r Response) Error() string {
	if s := pyTruthyStr(r.Body["error"]); s != "" {
		return s
	}
	return fmt.Sprintf("http_%d", r.Status)
}

// Message of the body, HTTP STATUS when the body has none
func (r Response) Message() string {
	if s := pyTruthyStr(r.Body["message"]); s != "" {
		return s
	}
	return fmt.Sprintf("HTTP %d", r.Status)
}

// Request that never produced an HTTP response
type TransportError struct{ Message string }

func (e *TransportError) Error() string { return e.Message }

// Configured plane origin and path prefix with trailing slashes dropped
func BaseURL() string {
	u := strings.TrimSpace(os.Getenv(URLEnv))
	if u == "" {
		u = DefaultURL
	}
	return strings.TrimRight(u, "/")
}

// Plane URL without user info for messages, describe and provenance
func ShownURL() string { return DisplayURL(BaseURL()) }

func InstanceID() string { return strings.TrimSpace(os.Getenv(InstanceEnv)) }

// Strict UTF-8 JSON in Python json.loads shapes with objects as string keyed maps
func loadJSON(raw []byte) (any, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("invalid utf-8")
	}
	v, err := pyjson.Loads(string(raw))
	if err != nil {
		return nil, err
	}
	return plainJSON(v), nil
}

func plainJSON(v any) any {
	switch c := v.(type) {
	case *pyjson.Object:
		out := make(map[string]any, c.Len())
		for _, k := range c.Keys {
			out[k] = plainJSON(c.Vals[k])
		}
		config.SetMappingKeys(out, c.Keys)
		return out
	case []any:
		out := make([]any, len(c))
		for i, e := range c {
			out[i] = plainJSON(e)
		}
		return out
	case int64:
		return int(c)
	case *big.Int:
		if c.IsInt64() {
			return int(c.Int64())
		}
		return c
	}
	return v
}

func parseBody(raw []byte) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	v, err := loadJSON(raw)
	if err != nil {
		return map[string]any{}
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Python float of a Retry-After header, false when empty or unparsable
func parseRetryAfter(value string) (float64, bool) {
	if value == "" {
		return 0, false
	}
	s := pystr.Strip(value)
	low := strings.ToLower(strings.TrimLeft(s, "+-"))
	if strings.HasPrefix(low, "0x") || strings.Contains(s, "__") || strings.HasPrefix(s, "_") || strings.HasSuffix(s, "_") {
		return 0, false
	}
	for i := 1; i < len(s)-1; i++ {
		if s[i] == '_' && !(isDigit(s[i-1]) && isDigit(s[i+1])) {
			return 0, false
		}
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return f, true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// Plane bearer of home, resolved only once the plane destination passed the policy
func Bearer(ctx context.Context, home string) (string, error) {
	if _, err := CheckedURL(BaseURL(), URLEnv); err != nil {
		return "", err
	}
	return PlaneToken(ctx, home)
}

// One call to the self route as the agent of home with token or a bearer resolved here, never following a redirect
func Request(ctx context.Context, method, home, profile, etag string, body any, token string) (Response, error) {
	plane, err := CheckedURL(BaseURL(), URLEnv)
	if err != nil {
		return Response{}, err
	}
	target := plane + "/v1/config/self?" + url.Values{"profile": {profile}}.Encode()
	if token == "" {
		if token, err = PlaneToken(ctx, home); err != nil {
			return Response{}, err
		}
	}
	var reader io.Reader
	var data string
	if body != nil {
		if data, err = pyjson.Dumps(body, pyjson.Separators(",", ":"), pyjson.EnsureASCII(false)); err != nil {
			return Response{}, err
		}
		reader = strings.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return Response{}, &TransportError{Message: fmt.Sprintf("%s %s: %v", method, target, err)}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Hermes-Instance-Id", InstanceID())
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := openCredentialed(req, httpTimeout)
	if err != nil {
		var derr *CredentialDestinationError
		if errors.As(err, &derr) {
			return Response{}, err
		}
		return Response{}, &TransportError{Message: fmt.Sprintf("%s %s: %v", method, target, err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, &TransportError{Message: fmt.Sprintf("%s %s: %v", method, target, err)}
	}
	out := Response{Status: resp.StatusCode, Body: parseBody(raw), ETag: resp.Header.Get("ETag")}
	out.RetryAfter, out.HasRetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	return out, nil
}

// Request context bounded by timeout, cancelled when the caller closes the body
func contextWithTimeout(req *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(req.Context(), timeout)
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// Python str of a truthy value, empty for a falsy one
func pyTruthyStr(v any) string {
	switch c := v.(type) {
	case nil:
		return ""
	case string:
		return c
	case bool:
		if c {
			return "True"
		}
		return ""
	case int:
		if c == 0 {
			return ""
		}
	case float64:
		if c == 0 {
			return ""
		}
	case map[string]any:
		if len(c) == 0 {
			return ""
		}
	case []any:
		if len(c) == 0 {
			return ""
		}
	}
	return pyrepr.Str(v)
}
