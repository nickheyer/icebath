package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const (
	URLEnv      = "ICEBATH_CONFIG_REMOTE_URL"
	InstanceEnv = "ICEBATH_CONFIG_INSTANCE_ID"
)

// Bound on one plane request, shortened by tests that drive the timeout paths
var httpTimeout = 15 * time.Second

// One plane answer, HasRetryAfter set when a Retry-After header held a delay in seconds or an HTTP date
type Response struct {
	Status        int
	Body          map[string]any
	ETag          string
	RetryAfter    float64
	HasRetryAfter bool
}

// Error code of the body, http_STATUS when the body names none
func (r Response) Error() string {
	if s := truthyText(r.Body["error"]); s != "" {
		return s
	}
	return fmt.Sprintf("http_%d", r.Status)
}

// Message of the body, HTTP STATUS when the body has none
func (r Response) Message() string {
	if s := truthyText(r.Body["message"]); s != "" {
		return s
	}
	return fmt.Sprintf("HTTP %d", r.Status)
}

// Request that never produced an HTTP response
type TransportError struct{ Message string }

func (e *TransportError) Error() string { return e.Message }

// Configured plane origin and path prefix with trailing slashes dropped, empty when none is set
func BaseURL() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv(URLEnv)), "/")
}

// Plane URL without user info for messages, describe and provenance, naming the unset variable when there is none
func ShownURL() string {
	if BaseURL() == "" {
		return "(" + URLEnv + " not set)"
	}
	return DisplayURL(BaseURL())
}

// Checked plane address, refused with the variable named when unset since Remote Config has no default plane
func planeURL() (string, error) {
	if BaseURL() == "" {
		return "", newCredentialDestinationError(URLEnv + " is not set; Remote Config has no default plane, " +
			"so set it to the https:// address of the config plane you run")
	}
	return CheckedURL(BaseURL(), URLEnv)
}

func InstanceID() string { return strings.TrimSpace(os.Getenv(InstanceEnv)) }

// Strict UTF-8 JSON in Python json.loads shapes with objects as string keyed maps
func loadJSON(raw []byte) (any, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("invalid utf-8")
	}
	v, err := jsonutil.Decode(string(raw))
	if err != nil {
		return nil, err
	}
	return plainJSON(v), nil
}

func plainJSON(v any) any {
	switch c := v.(type) {
	case *jsonutil.Object:
		out := make(map[string]any, c.Len())
		for _, k := range c.Keys() {
			out[k] = plainJSON(c.Value(k))
		}
		config.SetMappingKeys(out, c.Keys())
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

// Delay in seconds a Retry-After value writes as a Go float literal, space around it ignored and an overflowing value taken as the infinity of its sign; false when empty or not a number
func parseRetryAfter(value string) (float64, bool) {
	f, err := anyutil.ToFloat(value)
	if err != nil && !errors.Is(err, anyutil.ErrRange) {
		return 0, false
	}
	return f, true
}

// Seconds a Retry-After header asks to wait from now, its delay in seconds or the time left until its HTTP date; false when it holds neither
func retryAfterSeconds(value string, now time.Time) (float64, bool) {
	if f, ok := parseRetryAfter(value); ok {
		return f, true
	}
	if at, err := http.ParseTime(strings.TrimSpace(value)); err == nil {
		return at.Sub(now).Seconds(), true
	}
	return 0, false
}

// Body that ended before its declared length, the http.client.IncompleteRead no transport handler catches
type IncompleteRead struct {
	Partial     int
	Expected    int64
	HasExpected bool
}

func (e *IncompleteRead) Error() string {
	if e.HasExpected {
		return fmt.Sprintf("IncompleteRead(%d bytes read, %d more expected)", e.Partial, e.Expected)
	}
	return fmt.Sprintf("IncompleteRead(%d bytes read)", e.Partial)
}

// How far one request got, read when its failure is worded
type requestPhase struct {
	tlsStarted atomic.Bool
	connected  atomic.Bool
}

// Context that records the TLS handshake start and the connection handed to the request
func (p *requestPhase) trace(ctx context.Context) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		TLSHandshakeStart: func() { p.tlsStarted.Store(true) },
		GotConn:           func(httptrace.GotConnInfo) { p.connected.Store(true) },
	})
}

// Reason a request failed at the phase it reached: a timeout named by that phase, a server that closed the connection without answering, else the operating system reason or the transport's own text without the method and URL
func (p *requestPhase) reason(err error) string {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		switch {
		case p.connected.Load():
			return "timed out reading the response"
		case p.tlsStarted.Load():
			return "TLS handshake timed out"
		}
		return "timed out connecting"
	}
	if p.connected.Load() && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
		return "server closed the connection without a response"
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		err = ue.Err
	}
	return errchain.OSText(err)
}

// Plane bearer, resolved only once the plane destination passed the policy
func Bearer(ctx context.Context) (string, error) {
	if _, err := planeURL(); err != nil {
		return "", err
	}
	return planeToken(ctx)
}

// One call to the self route with token or a bearer resolved here, never following a redirect
func Request(ctx context.Context, method, profile, etag string, body any, token string) (Response, error) {
	plane, err := planeURL()
	if err != nil {
		return Response{}, err
	}
	target := plane + "/v1/config/self?" + url.Values{"profile": {profile}}.Encode()
	if token == "" {
		if token, err = planeToken(ctx); err != nil {
			return Response{}, err
		}
	}
	var reader io.Reader
	var data string
	if body != nil {
		if data, err = jsonutil.Encode(body); err != nil {
			return Response{}, err
		}
		reader = strings.NewReader(data)
	}
	phase := &requestPhase{}
	req, err := http.NewRequestWithContext(phase.trace(ctx), method, target, reader)
	if err != nil {
		return Response{}, &TransportError{Message: fmt.Sprintf("%s %s: %v", method, target, err)}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Icebath-Instance-Id", InstanceID())
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
		return Response{}, &TransportError{Message: fmt.Sprintf("%s %s: %s", method, target, phase.reason(err))}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return Response{}, &IncompleteRead{Partial: len(raw), Expected: resp.ContentLength - int64(len(raw)), HasExpected: resp.ContentLength >= 0}
	}
	if err != nil {
		return Response{}, &TransportError{Message: fmt.Sprintf("%s %s: %s", method, target, phase.reason(err))}
	}
	out := Response{Status: resp.StatusCode, Body: parseBody(raw), ETag: resp.Header.Get("ETag")}
	out.RetryAfter, out.HasRetryAfter = retryAfterSeconds(resp.Header.Get("Retry-After"), time.Now())
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

// Text of a truthy plane value, empty for null, false, zero and empty values
func truthyText(v any) string {
	if !anyutil.IsTrue(v) {
		return ""
	}
	return anyutil.String(v)
}
