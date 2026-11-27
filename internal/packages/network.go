package packages

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/httpx"
)

// Agent every asset host accepts, asking for bytes as stored
const userAgent = "Mozilla/5.0 (X11; Linux x86_64) icebath-pm/1.0"

// Tries of a request that keeps failing with no progress
const networkAttempts = 4

// HTTP client of every fetch: one HTTP/1.1 connection per range, no transparent decompression, https kept across redirects
var client = &http.Client{
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		DisableCompression:  true,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConnsPerHost: 2 * connections,
		IdleConnTimeout:     90 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		next := req.URL.String()
		if !strings.HasPrefix(next, "https://") && !(loopback(via[len(via)-1].URL.String()) && loopback(next)) {
			return &DownloadError{Kind: Failed, Message: "refusing redirect to non-https url: " + next}
		}
		return nil
	},
}

// Plain http on this machine, the one place http is allowed
func loopback(u string) bool {
	for _, prefix := range []string{"http://127.0.0.1:", "http://localhost:", "http://[::1]:"} {
		if strings.HasPrefix(u, prefix) {
			return true
		}
	}
	return false
}

// Response that was no success
type HTTPError struct {
	Code   int
	Reason string
	Header http.Header
	URL    string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP Error %d: %s", e.Code, e.Reason) }

// Body that ended before the bytes it declared
type shortRead struct{ expected int64 }

func (e *shortRead) Error() string {
	return fmt.Sprintf("the transfer ended %d bytes short", e.expected)
}

// Stall of a body that saw no bytes within its timeout
type stalled struct{}

func (stalled) Error() string   { return "the read timed out" }
func (stalled) Timeout() bool   { return true }
func (stalled) Temporary() bool { return true }

// Body whose every read renews its stall timer
type watched struct {
	io.ReadCloser
	timer   *time.Timer
	timeout time.Duration
	fired   *atomic.Bool
	cancel  context.CancelFunc
}

func (b *watched) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && b.fired.Load() {
		return n, stalled{}
	}
	b.timer.Reset(b.timeout)
	return n, err
}

func (b *watched) Close() error {
	b.timer.Stop()
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// Opens an address with the package manager's headers, cancelled when no byte comes within the timeout; anything but 2xx fails
func open(ctx context.Context, rawURL string, headers map[string]string, timeout time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Encoding", "identity")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	fired := new(atomic.Bool)
	timer := time.AfterFunc(timeout, func() { fired.Store(true); cancel() })
	resp, err := client.Do(req)
	if err != nil {
		timer.Stop()
		cancel()
		if de, ok := errors.AsType[*DownloadError](err); ok {
			return nil, de
		}
		if fired.Load() {
			return nil, &url.Error{Op: "Get", URL: rawURL, Err: stalled{}}
		}
		return nil, err
	}
	timer.Reset(timeout)
	resp.Body = &watched{ReadCloser: resp.Body, timer: timer, timeout: timeout, fired: fired, cancel: cancel}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		reason := http.StatusText(resp.StatusCode)
		if _, text, ok := strings.Cut(resp.Status, " "); ok && text != "" {
			reason = text
		}
		return nil, &HTTPError{Code: resp.StatusCode, Reason: reason, Header: resp.Header, URL: rawURL}
	}
	return resp, nil
}

// Whether a failure is a network condition worth trying again
func transient(err error) bool {
	if he, ok := errors.AsType[*HTTPError](err); ok {
		switch he.Code {
		case 408, 429, 500, 502, 503, 504:
			return true
		}
		return false
	}
	if tlsFailure(err) {
		return false
	}
	if dns, ok := errors.AsType[*net.DNSError](err); ok {
		return dns.IsTemporary || dns.IsTimeout
	}
	if _, ok := errors.AsType[*shortRead](err); ok {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		return true
	}
	for _, errno := range []syscall.Errno{syscall.ECONNABORTED, syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ENETDOWN, syscall.ENETRESET,
		syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.EPIPE, syscall.ETIMEDOUT} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// A certificate or handshake failure, which trying again never heals
func tlsFailure(err error) bool {
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verification *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var alert tls.AlertError
	return errors.As(err, &unknown) || errors.As(err, &hostname) || errors.As(err, &invalid) || errors.As(err, &verification) ||
		errors.As(err, &record) || errors.As(err, &alert)
}

// Why a 403 or 429 came from a CDN's firewall rather than the server, empty otherwise
func edgeBlock(err error) string {
	he, ok := errors.AsType[*HTTPError](err)
	if !ok || (he.Code != 403 && he.Code != 429) || he.Header == nil {
		return ""
	}
	cdn := ""
	switch {
	case he.Header.Get("x-vercel-mitigated") != "":
		cdn = "Vercel"
	case he.Header.Get("cf-mitigated") != "":
		cdn = "Cloudflare"
	default:
		return ""
	}
	who := "the server"
	if u, err := url.Parse(he.URL); err == nil && u.Hostname() != "" {
		who = strings.ToLower(u.Hostname())
	}
	return fmt.Sprintf("%s's CDN firewall (%s) refused this network. Your install is fine; retry from another network", who, cdn)
}

// Backoff of an attempt, a longer Retry-After of the server winning
func backoff(err error, attempt int) time.Duration {
	delay := math.Pow(2, float64(attempt))
	if he, ok := errors.AsType[*HTTPError](err); ok && he.Header != nil {
		if wait, ok := httpx.RetryAfter(he.Header.Get("Retry-After")); ok {
			delay = math.Max(delay, wait.Seconds())
		}
	}
	return time.Duration(delay * float64(time.Second))
}

// How a retried request waits and learns of progress
type retrying struct {
	// Bytes kept so far: progress gives the request a fresh budget of tries
	progress func() int64
	// A pinned mirror can take over, so a long server backoff is not waited out
	mirror bool
}

// Runs an idempotent request again on transient failures, waiting longer each time, kept bytes renewing its tries
func retry[T any](ctx context.Context, operation func() (T, error), opts retrying) (T, error) {
	failures, attempt := 0, 0
	var saved int64
	if opts.progress != nil {
		saved = opts.progress()
	}
	for {
		value, err := operation()
		if err == nil || !transient(err) || ctx.Err() != nil {
			return value, err
		}
		if current := saved; opts.progress != nil && opts.progress() > current {
			failures, attempt, saved = 0, 0, opts.progress()
		} else {
			failures++
		}
		if failures >= networkAttempts {
			return value, err
		}
		delay := backoff(err, attempt)
		limit := 30 * time.Second
		if opts.progress != nil && !opts.mirror {
			limit = 300 * time.Second
		}
		if opts.progress == nil {
			delay = min(delay, limit)
		} else if delay > limit {
			return value, err
		}
		attempt++
		slog.Warn(fmt.Sprintf("network request failed (%s); retrying in %gs (%d/%d failures without progress)", err, delay.Seconds(), failures, networkAttempts))
		select {
		case <-ctx.Done():
			return value, err
		case <-time.After(delay):
		}
	}
}
