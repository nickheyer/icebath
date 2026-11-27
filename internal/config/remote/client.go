package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/httpx"
)

// Variables that point icebath at the config plane and name this agent to it
const (
	URLEnv      = "ICEBATH_CONFIG_REMOTE_URL"
	InstanceEnv = "ICEBATH_CONFIG_INSTANCE_ID"
)

// Longest one request to the plane may take
const requestTimeout = 15 * time.Second

// Variable of the profile's environment, empty when it has none
type Env func(name string) string

// One answer of the plane
type response struct {
	status int
	body   map[string]any
	etag   string
	// Wait a Retry-After header asked for, and whether it asked one
	retryAfter    time.Duration
	hasRetryAfter bool
}

// Error code the body names, http_STATUS when it names none
func (r response) code() string {
	if s, _ := r.body["error"].(string); s != "" {
		return s
	}
	return fmt.Sprintf("http_%d", r.status)
}

// Message of the body, HTTP STATUS when it has none
func (r response) message() string {
	if s, _ := r.body["message"].(string); s != "" {
		return s
	}
	return fmt.Sprintf("HTTP %d", r.status)
}

// Request that never got an answer
type TransportError struct{ Message string }

func (e *TransportError) Error() string { return e.Message }

// Address of the plane, trailing slashes taken off, empty when none is set
func baseURL(env Env) string {
	return strings.TrimRight(strings.TrimSpace(env(URLEnv)), "/")
}

// Address of the plane as messages show it, naming the variable when none is set
func shownURL(env Env) string {
	if baseURL(env) == "" {
		return "(" + URLEnv + " not set)"
	}
	return DisplayURL(baseURL(env))
}

// Address of the plane once it passes the transport policy; Remote Config has no default plane
func planeURL(env Env) (string, error) {
	if baseURL(env) == "" {
		return "", destinationError(URLEnv + " is not set; Remote Config has no default plane, so set it to the https:// address of the config plane you run")
	}
	return checkedURL(baseURL(env), URLEnv)
}

// How far one request got, which says what a failure was
type phase struct {
	tls       atomic.Bool
	connected atomic.Bool
}

func (p *phase) trace(ctx context.Context) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		TLSHandshakeStart: func() { p.tls.Store(true) },
		GotConn:           func(httptrace.GotConnInfo) { p.connected.Store(true) },
	})
}

// Why the request failed, by how far it got
func (p *phase) reason(err error) string {
	if netErr, ok := errors.AsType[net.Error](err); errors.Is(err, context.DeadlineExceeded) || ok && netErr.Timeout() {
		switch {
		case p.connected.Load():
			return "timed out reading the response"
		case p.tls.Load():
			return "TLS handshake timed out"
		}
		return "timed out connecting"
	}
	if p.connected.Load() && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
		return "server closed the connection without a response"
	}
	if ue, ok := errors.AsType[*url.Error](err); ok && ue.Err != nil {
		err = ue.Err
	}
	// The system's own words, such as connect: connection refused
	if se, ok := errors.AsType[*os.SyscallError](err); ok {
		return se.Syscall + ": " + se.Err.Error()
	}
	return err.Error()
}

// Mapping of a JSON answer, empty for one that is not strict UTF-8 or not an object; whole numbers decode as integers, as the values of a
// config.yaml do
func decodeBody(raw []byte) map[string]any {
	if len(raw) == 0 || !utf8.Valid(raw) {
		return map[string]any{}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if decoder.Decode(&decoded) != nil {
		return map[string]any{}
	}
	body, ok := numbers(decoded).(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return body
}

// Value with its JSON numbers as integers when whole, else as floats
func numbers(value any) any {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			v[key] = numbers(item)
		}
	case []any:
		for i, item := range v {
			v[i] = numbers(item)
		}
	case json.Number:
		if n, err := strconv.ParseInt(v.String(), 10, 0); err == nil {
			return int(n)
		}
		f, _ := v.Float64()
		return f
	}
	return value
}

// One call to the profile's route of the plane with the bearer given, never following a redirect
func request(ctx context.Context, env Env, method, profile, etag string, body any, token string) (response, error) {
	plane, err := planeURL(env)
	if err != nil {
		return response{}, err
	}
	target := plane + "/v1/config/self?" + url.Values{"profile": {profile}}.Encode()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return response{}, err
		}
		reader = bytes.NewReader(data)
	}
	p := &phase{}
	req, err := http.NewRequestWithContext(p.trace(ctx), method, target, reader)
	if err != nil {
		return response{}, &TransportError{Message: fmt.Sprintf("%s %s: %v", method, target, err)}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Icebath-Instance-Id", strings.TrimSpace(env(InstanceEnv)))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := send(req, requestTimeout)
	if err != nil {
		if _, refused := errors.AsType[*DestinationError](err); refused {
			return response{}, err
		}
		return response{}, &TransportError{Message: fmt.Sprintf("%s %s: %s", method, target, p.reason(err))}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		ended := fmt.Sprintf("the answer ended after %d bytes", len(raw))
		if resp.ContentLength >= 0 {
			ended += fmt.Sprintf(", %d more expected", resp.ContentLength-int64(len(raw)))
		}
		return response{}, &TransportError{Message: fmt.Sprintf("%s %s: %s", method, target, ended)}
	}
	if err != nil {
		return response{}, &TransportError{Message: fmt.Sprintf("%s %s: %s", method, target, p.reason(err))}
	}
	out := response{status: resp.StatusCode, body: decodeBody(raw), etag: resp.Header.Get("ETag")}
	out.retryAfter, out.hasRetryAfter = httpx.RetryAfter(resp.Header.Get("Retry-After"))
	return out, nil
}
