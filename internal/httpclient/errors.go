package httpclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Stage of an exchange where a failure happened
type Phase uint8

// Exchange stages in the order a request passes through them
const (
	PhasePool Phase = iota + 1
	PhaseConnect
	PhaseProxy
	PhaseTLS
	PhaseWrite
	PhaseHeader
	PhaseBody
	PhaseDecode
)

// Verb phrase naming the stage in error text
func (p Phase) String() string {
	switch p {
	case PhasePool:
		return "waiting for a connection to"
	case PhaseConnect:
		return "connecting to"
	case PhaseProxy:
		return "proxying to"
	case PhaseTLS:
		return "TLS handshake with"
	case PhaseWrite:
		return "sending request to"
	case PhaseHeader:
		return "awaiting response headers from"
	case PhaseBody:
		return "reading response body from"
	case PhaseDecode:
		return "decoding response body from"
	}
	return "exchanging with"
}

// Matches every deadline that ended an exchange, the caller context deadline included, checked with errors.Is
var ErrTimeout = errors.New("timed out")

// Returned when a request URL scheme is neither http nor https
var ErrUnsupportedScheme = errors.New("request URL scheme is not http or https")

// Returned when a client follows more redirects than its limit
var ErrTooManyRedirects = errors.New("too many redirects")

// Deadline of one phase that expired after the configured duration
type deadlineError struct{ after time.Duration }

func (e *deadlineError) Error() string { return "timed out after " + e.after.String() }

func (e *deadlineError) Is(target error) bool { return target == ErrTimeout }

func (e *deadlineError) Timeout() bool { return true }

// Failed exchange with the phase it failed in and the host and port it targeted
type Error struct {
	Phase Phase
	Host  string
	Err   error
}

func (e *Error) Error() string { return e.Phase.String() + " " + e.Host + ": " + e.Err.Error() }

func (e *Error) Unwrap() error { return e.Err }

// Reports whether the phase ran out of time rather than failing outright
func (e *Error) Timeout() bool {
	if errors.Is(e.Err, ErrTimeout) || errors.Is(e.Err, context.DeadlineExceeded) || errors.Is(e.Err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(e.Err, &ne) && ne.Timeout()
}

// Lets the errors package match ErrTimeout for every kind of phase deadline
func (e *Error) Is(target error) bool { return target == ErrTimeout && e.Timeout() }

// Failure reaching a proxy or negotiating a tunnel, with the reply status when the proxy answered
type ProxyError struct {
	Proxy  string
	Status int
	Err    error
}

func (e *ProxyError) Error() string {
	if e.Status != 0 {
		return "proxy " + e.Proxy + " refused the tunnel with " + e.Err.Error()
	}
	return "proxy " + e.Proxy + ": " + e.Err.Error()
}

func (e *ProxyError) Unwrap() error { return e.Err }

// Response whose status is a client or server error
type StatusError struct {
	Code   int
	Status string
	Method string
	Host   string
}

func (e *StatusError) Error() string { return e.Method + " " + e.Host + ": " + e.Status }

// Error for a 4xx or 5xx response, nil for any other status
func CheckStatus(resp *http.Response) error {
	if resp.StatusCode < 400 {
		return nil
	}
	method, host := http.MethodGet, ""
	if resp.Request != nil {
		if resp.Request.Method != "" {
			method = resp.Request.Method
		}
		if resp.Request.URL != nil {
			host = resp.Request.URL.Host
		}
	}
	status := resp.Status
	if status == "" {
		status = fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return &StatusError{Code: resp.StatusCode, Status: status, Method: method, Host: host}
}

// Error text with every request URL cut to its scheme and host, safe to show users and write to logs
func Message(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	for e := err; e != nil; e = errors.Unwrap(e) {
		if ue, ok := e.(*url.Error); ok {
			text = strings.ReplaceAll(text, strconv.Quote(ue.URL), originText(ue.URL))
		}
	}
	return text
}

// Scheme and host of a serialized URL with userinfo, path, query and fragment removed
func originText(raw string) string {
	scheme, rest := "", raw
	if i := strings.Index(rest, "://"); i >= 0 {
		scheme, rest = strings.ToLower(rest[:i+3]), rest[i+3:]
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	return scheme + rest
}
