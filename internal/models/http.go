package models

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/internal/provider"
)

// Longest any one request of a catalog or probe takes, its whole body included
const requestDeadline = 15 * time.Second

// Most of a body read
const bodyLimit = 64 << 20

// Kind whose routes list their models at a fixed address, or at the models path of their base URL when none is given; a public list is
// asked for without the route's key
type Listed struct {
	Provider string
	URL      string
	Public   bool
}

func (l Listed) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	if route.Spec != l.Provider {
		return nil, false
	}
	if l.Public {
		route.APIKey = ""
	}
	return List(ctx, route, l.URL)
}

// Chat model ids of an OpenAI style models listing, at the address or at the models path of the route's base URL
func List(ctx context.Context, route provider.Route, modelsURL string) ([]string, bool) {
	target := strings.TrimSpace(modelsURL)
	if target == "" {
		if route.BaseURL == "" {
			return nil, false
		}
		target = strings.TrimRight(route.BaseURL, "/") + "/models"
	}
	headers := map[string]string{"Accept": "application/json", "User-Agent": provider.UserAgent("icebath-cli")}
	if route.APIKey != "" {
		headers["Authorization"] = "Bearer " + route.APIKey
	}
	maps.Copy(headers, route.Headers)
	data, err := GetJSON(ctx, target, 8*time.Second, headers)
	if err != nil {
		return nil, false
	}
	items := data
	if document, ok := data.(map[string]any); ok {
		items = document["data"]
	} else if _, ok := data.([]any); !ok {
		noted(ctx, func(u *Unlisted) { u.URL, u.Shape = target, true })
		return nil, false
	}
	return ChatCatalogIDs(items), true
}

// Why a provider gave no list of models of its own, as the requests of its listing met it: the address asked last, and the error status it
// answered with, the failure to reach it or an answer that holds no list; an answer with no models when none of those
type Unlisted struct {
	URL    string
	Status int
	Err    error
	Shape  bool
}

// Context key of the Unlisted a listing notes what its requests met in
type unlistedKey struct{}

// Context whose requests note what they meet in the Unlisted
func noting(ctx context.Context, unlisted *Unlisted) context.Context {
	return context.WithValue(ctx, unlistedKey{}, unlisted)
}

// Notes what a request of a listing met, when the context is one's
func noted(ctx context.Context, note func(*Unlisted)) {
	if unlisted, ok := ctx.Value(unlistedKey{}).(*Unlisted); ok {
		note(unlisted)
	}
}

// Why the provider with the name has no models of its own to pick, then what to do, in sentences a person reads
func (u Unlisted) Reason(name string) string {
	at := ""
	if u.URL != "" {
		at = " at " + u.URL
	}
	switch {
	case u.Err != nil:
		return fmt.Sprintf("icebath could not reach %s%s, because %s. Check its address and that the server runs, then try again.", name, at,
			unreached(u.Err))
	case u.Status == http.StatusUnauthorized || u.Status == http.StatusForbidden:
		return fmt.Sprintf("%s refused to list its models%s, with HTTP status %d. Check its API key.", name, at, u.Status)
	case u.Status == http.StatusNotFound:
		return fmt.Sprintf("%s has no list of models%s, as it answered with HTTP status 404. Check its address.", name, at)
	case u.Status != 0:
		return fmt.Sprintf("%s answered with HTTP status %d when asked for its models%s. Try again later.", name, u.Status, at)
	case u.Shape:
		return fmt.Sprintf("%s answered%s with something other than a list of models. Check its address.", name, at)
	}
	return fmt.Sprintf("%s lists no models%s.", name, at)
}

// What reaching an address failed on, as the end of a sentence
func unreached(err error) string {
	var dns *net.DNSError
	var certificate *tls.CertificateVerificationError
	switch {
	case errors.As(err, &dns):
		return "its host name was not found"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "nothing answered there"
	case errors.As(err, &certificate):
		return "its certificate is not trusted"
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded):
		return "it did not answer in time"
	}
	if failure, ok := errors.AsType[net.Error](err); ok && failure.Timeout() {
		return "it did not answer in time"
	}
	text := err.Error()
	if i := strings.LastIndex(text, ": "); i >= 0 {
		text = text[i+2:]
	}
	return "of this error: " + text
}

// JSON a GET of the address answers with, an error for any status but success
func GetJSON(ctx context.Context, target string, timeout time.Duration, headers map[string]string) (any, error) {
	reply, err := send(ctx, http.MethodGet, target, headers, nil, timeout, true)
	if err != nil {
		return nil, err
	}
	if !reply.ok() {
		return nil, &StatusError{Status: reply.status, URL: target}
	}
	data, err := reply.json()
	if err != nil {
		noted(ctx, func(u *Unlisted) { u.Shape = true })
	}
	return data, err
}

// Non success status of a request
type StatusError struct {
	Status int
	URL    string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s answered with HTTP status %d", e.URL, e.Status)
}

// Status of an error a request failed with, 0 when it got no answer
func Status(err error) int {
	if status, ok := errors.AsType[*StatusError](err); ok {
		return status.Status
	}
	return 0
}

// What one request was answered with
type answer struct {
	status int
	header http.Header
	body   []byte
}

func (a *answer) ok() bool {
	return a.status >= 200 && a.status < 300
}

// Body as any JSON value
func (a *answer) json() (any, error) {
	var data any
	if err := json.Unmarshal(bytes.TrimPrefix(a.body, []byte("\xef\xbb\xbf")), &data); err != nil {
		return nil, err
	}
	return data, nil
}

// Body when it is a JSON object, else an empty one
func (a *answer) object() map[string]any {
	data, _ := a.json()
	if object, ok := data.(map[string]any); ok {
		return object
	}
	return map[string]any{}
}

// Sends one request under the timeout for connecting and for each read, and the deadline for the whole of it. A redirect is followed when
// asked, credentials dropped on the way to another origin; else the redirect is the answer
func send(ctx context.Context, method, target string, headers map[string]string, body []byte, timeout time.Duration, follow bool) (*answer, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, min(requestDeadline, 3*timeout))
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(headers)) {
		request.Header.Set(name, headers[name])
	}
	if body != nil && request.Header.Get("Content-Type") == "" {
		request.Header.Set("Content-Type", "application/json")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: min(timeout, 5*time.Second), KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = min(timeout, 5*time.Second)
	transport.ResponseHeaderTimeout = timeout
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(next *http.Request, via []*http.Request) error {
		if !follow {
			return http.ErrUseLastResponse
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects, so the address may loop")
		}
		if origin(next.URL) != origin(via[0].URL) {
			for name := range headers {
				if lower := strings.ToLower(name); lower != "accept" && lower != "user-agent" {
					next.Header.Del(name)
				}
			}
			next.Header.Del("Authorization")
			next.Header.Del("Cookie")
		}
		return nil
	}}
	response, err := client.Do(request)
	if err != nil {
		noted(ctx, func(u *Unlisted) { *u = Unlisted{URL: target, Err: err} })
		return nil, err
	}
	defer response.Body.Close()
	noted(ctx, func(u *Unlisted) {
		*u = Unlisted{URL: target}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			u.Status = response.StatusCode
		}
	})
	stream := io.Reader(response.Body)
	if strings.EqualFold(response.Header.Get("Content-Encoding"), "gzip") && !response.Uncompressed {
		zipped, err := gzip.NewReader(response.Body)
		if err != nil {
			return nil, err
		}
		stream = zipped
	}
	data, err := io.ReadAll(io.LimitReader(stream, bodyLimit))
	if err != nil {
		return nil, err
	}
	return &answer{status: response.StatusCode, header: response.Header, body: data}, nil
}

func origin(u *url.URL) string {
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// Whether the request failed while connecting, which a server that took the connection and then went quiet does not
func connectTimeout(err error) bool {
	if op, ok := errors.AsType[*net.OpError](err); ok {
		return op.Op == "dial" && op.Timeout()
	}
	return false
}

// Bearer header of a key, none without one
func bearer(key string) map[string]string {
	if key = strings.TrimSpace(key); key == "" {
		return map[string]string{}
	}
	return map[string]string{"Authorization": "Bearer " + key}
}

// JSON object one request is answered with, empty for an empty body; a StatusError for any status but success. Redirects are followed,
// without the credentials when they lead to another origin
func Request(ctx context.Context, method, target string, headers map[string]string, body []byte, timeout time.Duration) (map[string]any, error) {
	reply, err := send(ctx, method, target, headers, body, timeout, true)
	if err != nil {
		return nil, err
	}
	if !reply.ok() {
		return nil, &StatusError{Status: reply.status, URL: target}
	}
	if len(bytes.TrimSpace(reply.body)) == 0 {
		return map[string]any{}, nil
	}
	data, err := reply.json()
	if err != nil {
		return nil, err
	}
	switch v := data.(type) {
	case map[string]any:
		return v, nil
	case nil:
		return map[string]any{}, nil
	}
	return nil, fmt.Errorf("%s answered with something other than a set of fields", target)
}
