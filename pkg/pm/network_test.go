package pm

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// HTTP error for the fixture tool URL with an optional Retry-After header
func dltHTTPError(status int, retryAfter string) *HTTPError {
	header := http.Header{}
	if retryAfter != "" {
		header.Set("Retry-After", retryAfter)
	}
	return &HTTPError{Code: status, Reason: "failed", Header: header, URL: "https://example.invalid/tool"}
}

func TestOnlyTransientFailuresUseAFiniteRetryBudget(t *testing.T) {
	type scenario struct {
		id      string
		err     error
		retried bool
	}
	var cases []scenario
	for _, code := range []int{408, 429, 500, 502, 503, 504} {
		cases = append(cases, scenario{fmt.Sprintf("http-%d", code), dltHTTPError(code, ""), true})
	}
	for _, code := range []int{400, 401, 403, 404, 410, 501, 505} {
		cases = append(cases, scenario{fmt.Sprintf("http-%d", code), dltHTTPError(code, ""), false})
	}
	certificate := &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}
	cases = append(cases,
		scenario{"connect-timeout", &url.Error{Op: "Get", URL: "https://example.invalid/tool", Err: socketTimeout{}}, true},
		scenario{"read-timeout", socketTimeout{}, true},
		scenario{"connection-reset", &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, true},
		scenario{"remote-close", &RemoteDisconnectedError{Message: "closed"}, true},
		scenario{"short-read", &IncompleteReadError{Partial: 6, Expected: 100}, true},
		scenario{"temporary-dns", &net.DNSError{Err: "temporary", Name: "example.invalid", IsTemporary: true}, true},
		scenario{"invalid-host", &net.DNSError{Err: "no name", Name: "example.invalid", IsNotFound: true}, false},
		scenario{"tls-eof", &url.Error{Op: "Get", URL: "https://example.invalid/tool", Err: io.EOF}, true},
		scenario{"tls-certificate", certificate, false},
		scenario{"wrapped-certificate", &url.Error{Op: "Get", URL: "https://example.invalid/tool", Err: certificate}, false},
		scenario{"disk-full", &os.PathError{Op: "write", Path: "full", Err: syscall.ENOSPC}, false},
		scenario{"filesystem-denied", &os.PathError{Op: "open", Path: "denied", Err: fs.ErrPermission}, false},
		scenario{"hash", downloadError(DownloadHashFailed, "bad pin"), false},
		scenario{"pause", downloadError(DownloadWasPaused, "pause"), false},
		scenario{"interrupt", context.Canceled, false},
		scenario{"invalid-data", errors.New("bad archive"), false},
	)
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			attempts := 0
			var waits []time.Duration
			_, err := RetryNetwork(func() (struct{}, error) {
				attempts++
				return struct{}{}, c.err
			}, RetryOptions{Wait: func(d time.Duration) error { waits = append(waits, d); return nil }})
			if err != c.err {
				t.Fatalf("raised %v, want the original %v", err, c.err)
			}
			expected := 1
			if c.retried {
				expected = networkAttempts
			}
			if attempts != expected || len(waits) != expected-1 {
				t.Fatalf("attempts %d waits %v", attempts, waits)
			}
			for _, d := range waits {
				if d <= 0 || d > time.Duration(maxDelaySeconds*float64(time.Second)) {
					t.Fatalf("delay %v out of bounds", d)
				}
			}
			if !slices.IsSorted(waits) {
				t.Fatalf("delays %v not ascending", waits)
			}
		})
	}
}

func TestRetryAfterIsBoundedAndInterruptible(t *testing.T) {
	now := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		id, retryAfter string
		minimum        float64
	}{
		{"none", "", 1},
		{"7", "7", 7},
		{"-5", "-5", 1},
		{"not-a-date", "not a date", 1},
		{"oversized-delay", strings.Repeat("9", 400), maxDelaySeconds},
		{"future-date", now.Add(12 * time.Second).Format(http.TimeFormat), 12},
		{"expired-date", now.Add(-12 * time.Second).Format(http.TimeFormat), 1},
	}
	dltSwap(t, &networkNow, func() time.Time { return now })
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			failure := dltHTTPError(503, c.retryAfter)
			attempts := 0
			var waits []time.Duration
			_, err := RetryNetwork(func() (struct{}, error) {
				attempts++
				return struct{}{}, failure
			}, RetryOptions{Wait: func(d time.Duration) error {
				waits = append(waits, d)
				return downloadError(DownloadWasPaused, "cancelled during backoff")
			}})
			if !IsDownloadKind(err, DownloadWasPaused) {
				t.Fatalf("want the pause from the wait, got %v", err)
			}
			if attempts != 1 || len(waits) != 1 {
				t.Fatalf("attempts %d waits %v", attempts, waits)
			}
			seconds := waits[0].Seconds()
			if seconds < c.minimum || seconds > maxDelaySeconds {
				t.Fatalf("delay %gs outside %g to %g", seconds, c.minimum, maxDelaySeconds)
			}
		})
	}
}

func TestDownloadFailureNamesOnlyACDNFirewallRefusal(t *testing.T) {
	cases := []struct {
		id      string
		status  int
		headers map[string]string
		cdn     string
	}{
		{"vercel-deny", 403, map[string]string{"Server": "Vercel", "x-vercel-mitigated": "deny"}, "Vercel"},
		{"vercel-challenge-429", 429, map[string]string{"Server": "Vercel", "x-vercel-mitigated": "challenge"}, "Vercel"},
		{"cloudflare-challenge", 403, map[string]string{"Server": "cloudflare", "cf-mitigated": "challenge", "cf-cache-status": "DYNAMIC"}, "Cloudflare"},
		{"cloudflare-origin-not-r2", 403, map[string]string{"Server": "cloudflare"}, ""},
		{"missing-object", 404, map[string]string{"Server": "cloudflare"}, ""},
		{"origin-403", 403, map[string]string{"Server": "AmazonS3"}, ""},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			header := http.Header{}
			for k, v := range c.headers {
				header.Set(k, v)
			}
			rawURL := "https://mirror.example/x"
			message := NewDownloadTransportError(rawURL, &HTTPError{Code: c.status, Reason: "failed", Header: header, URL: rawURL}).Error()
			if c.cdn == "" {
				if strings.Contains(message, "CDN firewall") {
					t.Fatalf("origin refusal blamed on a CDN: %s", message)
				}
				return
			}
			if !strings.Contains(message, "mirror.example's CDN firewall ("+c.cdn+") refused this network") {
				t.Fatalf("message %s", message)
			}
		})
	}
}

func TestResumableTransferHonorsRetryAfterOrStops(t *testing.T) {
	cases := []struct {
		retryAfter string
		expected   time.Duration
	}{{"60", 60 * time.Second}, {"600", 0}}
	for _, c := range cases {
		t.Run(c.retryAfter, func(t *testing.T) {
			attempts := 0
			var waits []time.Duration
			value, err := RetryNetwork(func() (string, error) {
				attempts++
				if attempts == 1 {
					return "", dltHTTPError(503, c.retryAfter)
				}
				return "complete", nil
			}, RetryOptions{Wait: func(d time.Duration) error { waits = append(waits, d); return nil }, Progress: func() int64 { return 0 }})
			if c.expected == 0 {
				var httpErr *HTTPError
				if !errors.As(err, &httpErr) {
					t.Fatalf("want the HTTP error, got %v", err)
				}
				if attempts != 1 || len(waits) != 0 {
					t.Fatalf("attempts %d waits %v", attempts, waits)
				}
				return
			}
			if err != nil || value != "complete" {
				t.Fatalf("value %q error %v", value, err)
			}
			if !slices.Equal(waits, []time.Duration{c.expected}) {
				t.Fatalf("waits %v", waits)
			}
		})
	}
}
