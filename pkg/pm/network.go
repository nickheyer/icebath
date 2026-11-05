package pm

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const (
	networkAttempts      = 4
	maxDelaySeconds      = 30.0
	maxRecoveryDelaySecs = 300.0
)

var httpRetryCodes = map[int]bool{408: true, 429: true, 500: true, 502: true, 503: true, 504: true}

var r2Hosts = map[string]bool{"hermes-assets.nousresearch.com": true}

var networkErrnos = []syscall.Errno{
	syscall.ECONNABORTED, syscall.ECONNREFUSED, syscall.ECONNRESET,
	syscall.ENETDOWN, syscall.ENETRESET, syscall.ENETUNREACH,
	syscall.EHOSTUNREACH, syscall.EPIPE, syscall.ETIMEDOUT,
}

// HTTPError is a non success response, carrying its status, reason, headers and request URL
type HTTPError struct {
	Code   int
	Reason string
	Header http.Header
	URL    string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP Error %d: %s", e.Code, e.Reason) }

// Builds an HTTP error from a response whose body the caller already drained or closed
func httpErrorFrom(rawURL string, resp *http.Response) *HTTPError {
	reason := http.StatusText(resp.StatusCode)
	if _, text, ok := strings.Cut(resp.Status, " "); ok && text != "" {
		reason = text
	}
	return &HTTPError{Code: resp.StatusCode, Reason: reason, Header: resp.Header, URL: rawURL}
}

// IncompleteReadError is a body that ended before its declared length
type IncompleteReadError struct {
	Partial  int64
	Expected int64
}

func (e *IncompleteReadError) Error() string {
	if e.Expected > 0 {
		return fmt.Sprintf("IncompleteRead(%d bytes read, %d more expected)", e.Partial, e.Expected)
	}
	return fmt.Sprintf("IncompleteRead(%d bytes read)", e.Partial)
}

// RemoteDisconnectedError is a connection the server closed before sending a response
type RemoteDisconnectedError struct{ Message string }

func (e *RemoteDisconnectedError) Error() string { return e.Message }

// Reports whether a failure is a transient network condition worth retrying
func IsTransient(err error) bool {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpRetryCodes[httpErr.Code]
	}
	if isTLSFailure(err) {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsTemporary || dnsErr.IsTimeout
	}
	var incomplete *IncompleteReadError
	var disconnected *RemoteDisconnectedError
	if errors.As(err, &incomplete) || errors.As(err, &disconnected) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && errors.Is(urlErr.Err, io.EOF) {
		return true
	}
	for _, errno := range networkErrnos {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// Reports a TLS failure other than an unexpected end of stream, which never heals by retrying
func isTLSFailure(err error) bool {
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verification *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var alert tls.AlertError
	var systemRoots x509.SystemRootsError
	return errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalid) ||
		errors.As(err, &verification) || errors.As(err, &record) || errors.As(err, &alert) || errors.As(err, &systemRoots)
}

// Explains a 403 or 429 that a CDN firewall sent instead of the server, empty otherwise
func EdgeBlock(err error) string {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || (httpErr.Code != 403 && httpErr.Code != 429) {
		return ""
	}
	headers := httpErr.Header
	if headers == nil {
		headers = http.Header{}
	}
	host := ""
	if u, perr := url.Parse(httpErr.URL); perr == nil {
		host = strings.ToLower(u.Hostname())
	}
	var cdn string
	switch {
	case headers.Get("x-vercel-mitigated") != "":
		cdn = "Vercel"
	case headers.Get("cf-mitigated") != "" || (r2Hosts[host] && strings.ToLower(headers.Get("server")) == "cloudflare" && headers.Get("cf-cache-status") == ""):
		cdn = "Cloudflare"
	default:
		return ""
	}
	who := host
	if who == "" {
		who = "the server"
	}
	return fmt.Sprintf("%s's CDN firewall (%s) refused this network. Your install is fine; retry from another network", who, cdn)
}

var pyIntLiteral = regexp.MustCompile(`^\s*[+-]?[0-9]+(_[0-9]+)*\s*$`)

// Clock the retry policy reads, replaceable by tests
var networkNow = time.Now

// Backoff seconds for an attempt, a larger Retry-After from an HTTP error winning
func retryDelay(err error, attempt int) float64 {
	delay := math.Pow(2, float64(attempt))
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Header == nil {
		return delay
	}
	value := httpErr.Header.Get("Retry-After")
	if value == "" {
		return delay
	}
	var retryAfter float64
	if pyIntLiteral.MatchString(value) {
		n, _ := new(big.Float).SetString(strings.ReplaceAll(strings.TrimSpace(value), "_", ""))
		retryAfter, _ = n.Float64()
	} else if when, perr := parseHTTPDate(value); perr == nil {
		retryAfter = float64(when.UnixNano())/1e9 - float64(networkNow().UnixNano())/1e9
	}
	return math.Max(delay, retryAfter)
}

// Parses an RFC 2822 date, a zone less date read as UTC
func parseHTTPDate(value string) (time.Time, error) {
	if t, err := mail.ParseDate(value); err == nil {
		return t, nil
	}
	return http.ParseTime(value)
}

// RetryOptions shape a retried network operation
type RetryOptions struct {
	Wait              func(time.Duration) error
	Progress          func() int64
	FallbackAvailable bool
}

// Retries a whole idempotent request on transient failures, durable progress renewing the budget
func RetryNetwork[T any](operation func() (T, error), opts RetryOptions) (T, error) {
	failuresSinceProgress := 0
	backoff := 0
	var saved int64
	if opts.Progress != nil {
		saved = opts.Progress()
	}
	wait := opts.Wait
	if wait == nil {
		wait = func(d time.Duration) error { time.Sleep(d); return nil }
	}
	for {
		value, err := operation()
		if err == nil {
			return value, nil
		}
		if !IsTransient(err) {
			return value, err
		}
		current := saved
		if opts.Progress != nil {
			current = opts.Progress()
		}
		if current > saved {
			failuresSinceProgress = 0
			backoff = 0
			saved = current
		} else {
			failuresSinceProgress++
		}
		if failuresSinceProgress >= networkAttempts {
			return value, err
		}
		delay := retryDelay(err, backoff)
		if opts.Progress == nil {
			delay = math.Min(delay, maxDelaySeconds)
		} else {
			limit := maxRecoveryDelaySecs
			if opts.FallbackAvailable {
				limit = maxDelaySeconds
			}
			if delay > limit {
				return value, err
			}
		}
		backoff++
		slog.Warn(fmt.Sprintf("network request failed (%s); retrying in %gs (%d/%d failures without progress)",
			err, delay, failuresSinceProgress, networkAttempts))
		if werr := wait(time.Duration(delay * float64(time.Second))); werr != nil {
			return value, werr
		}
	}
}
