package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
)

const (
	DefaultURL  = "https://config-config.icebath.app"
	EnvURL      = "ICEBATH_CONFIG_REMOTE_URL"
	EnvInstance = "ICEBATH_CONFIG_INSTANCE_ID"
	httpTimeout = 15 * time.Second
)

type credentialError struct {
	msg       string
	retryable bool
	err       error
}

func (e *credentialError) Error() string { return e.msg }

func (e *credentialError) Unwrap() error { return e.err }

type transportError struct {
	msg string
	err error
}

func (e *transportError) Error() string { return e.msg }

func (e *transportError) Unwrap() error { return e.err }

type response struct {
	status     int
	body       map[string]json.RawMessage
	etag       string
	retryAfter time.Duration
}

func rawString(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func rawText(raw json.RawMessage) string {
	if s, ok := rawString(raw); ok {
		return s
	}
	return strings.TrimSpace(string(raw))
}

func rawTruthy(raw json.RawMessage) bool {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return false
	}
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

func rawInt(raw json.RawMessage) (int64, bool) {
	var n json.Number
	if len(raw) == 0 || json.Unmarshal(raw, &n) != nil || strings.ContainsAny(n.String(), ".eE") {
		return 0, false
	}
	v, err := n.Int64()
	return v, err == nil
}

func (r *response) field(name string) string {
	if raw, ok := r.body[name]; ok && rawTruthy(raw) {
		return rawText(raw)
	}
	return ""
}

func (r *response) code() string {
	if t := r.field("error"); t != "" {
		return t
	}
	return fmt.Sprintf("http_%d", r.status)
}

func (r *response) message() string {
	if t := r.field("message"); t != "" {
		return t
	}
	return fmt.Sprintf("HTTP %d", r.status)
}

func baseURL() string {
	v := strings.TrimSpace(os.Getenv(EnvURL))
	if v == "" {
		v = DefaultURL
	}
	return strings.TrimRight(v, "/")
}

func shownURL() string { return displayURL(baseURL()) }

func instanceID() string { return strings.TrimSpace(os.Getenv(EnvInstance)) }

func parseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n > 65535 {
			return nil, errors.New("Port out of range 0-65535")
		}
	}
	return u, nil
}

func displayURL(raw string) string {
	u, err := parseURL(raw)
	if err != nil {
		if head, tail, found := strings.Cut(raw, "//"); found {
			return head + "//" + tail[strings.LastIndex(tail, "@")+1:]
		}
		return raw[strings.LastIndex(raw, "@")+1:]
	}
	if u.User == nil {
		return raw
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if p := u.Port(); p != "" {
		host += ":" + p
	}
	u.User, u.Host = nil, host
	return u.String()
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.IsLoopback()
}

func destinationError(msg string) error { return &credentialError{msg: msg} }

func checkedURL(raw, envName string) (string, error) {
	u, err := parseURL(raw)
	if err != nil {
		return "", destinationError(fmt.Sprintf("%s=%s is not a valid URL: %v", envName, quote(displayURL(raw)), err))
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", destinationError(fmt.Sprintf("%s=%s has no host.", envName, quote(displayURL(raw))))
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", destinationError(fmt.Sprintf("%s must be a plain origin and path (no user info, query or fragment); "+
			"Remote Config does not send credentials to an ambiguous destination.", envName))
	}
	if u.Scheme == "https" || (u.Scheme == "http" && isLoopback(host)) {
		return raw, nil
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "no"
	}
	return "", destinationError(fmt.Sprintf("%s must be an https:// URL (plain http is accepted only for localhost, "+
		"127.0.0.1 or ::1); Remote Config does not send credentials to %s://%s.", envName, scheme, host))
}

func (b *Backend) httpClient() (*http.Client, error) {
	b.clientMu.Lock()
	defer b.clientMu.Unlock()
	if b.client != nil {
		return b.client, nil
	}
	t, err := httpclient.New(httpclient.Options{Timeouts: httpclient.EachPhase(httpTimeout)})
	if err != nil {
		return nil, err
	}
	c := t.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	b.client = c
	return c, nil
}

func (b *Backend) send(req *http.Request) (*http.Response, []byte, error) {
	c, err := b.httpClient()
	if err != nil {
		return nil, nil, &transportError{msg: fmt.Sprintf("%s %s: %v", req.Method, req.URL, err), err: err}
	}
	resp, err := c.Do(req)
	if err != nil {
		reason := err
		var ue *url.Error
		if errors.As(err, &ue) {
			reason = ue.Err
		}
		return nil, nil, &transportError{msg: fmt.Sprintf("%s %s: %v", req.Method, req.URL, reason), err: err}
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return nil, nil, destinationError(fmt.Sprintf("%s answered HTTP %d redirecting to %s; Remote Config never follows a "+
			"redirect with credentials. Point the URL at the final destination.", req.URL, resp.StatusCode,
			quote(displayURL(resp.Header.Get("Location")))))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, &transportError{msg: fmt.Sprintf("%s %s: %v", req.Method, req.URL, err), err: err}
	}
	return resp, data, nil
}

func (b *Backend) request(ctx context.Context, method, h, profile, etag string, body []byte, token string) (*response, error) {
	plane, err := checkedURL(baseURL(), EnvURL)
	if err != nil {
		return nil, err
	}
	if token == "" {
		if token, err = b.planeToken(ctx, h); err != nil {
			return nil, err
		}
	}
	target := plane + "/v1/config/self?" + url.Values{"profile": {profile}}.Encode()
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return nil, &transportError{msg: fmt.Sprintf("%s %s: %v", method, target, err), err: err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Icebath-Instance-Id", instanceID())
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, data, err := b.send(req)
	if err != nil {
		return nil, err
	}
	out := &response{status: resp.StatusCode, body: map[string]json.RawMessage{}, etag: resp.Header.Get("ETag")}
	if len(data) > 0 && json.Unmarshal(data, &out.body) != nil {
		out.body = map[string]json.RawMessage{}
	}
	if v := strings.TrimSpace(resp.Header.Get("Retry-After")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			out.retryAfter = time.Duration(min(f, maxRetryWait.Seconds()) * float64(time.Second))
		}
	}
	return out, nil
}
