package connections

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// Keep alive pools of the JSON calls and of the streaming downloads, kept apart so downloads never starve JSON
var (
	jsonTransport     = newPooledTransport(50)
	downloadTransport = newPooledTransport(8)
)

func newPooledTransport(maxConns int) *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext,
		MaxConnsPerHost:       maxConns,
		MaxIdleConnsPerHost:   maxConns,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   0,
		ExpectContinueTimeout: 0,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
}

// DownloadTransport is the pooled transport of streaming gateway downloads
func DownloadTransport() *http.Transport { return downloadTransport }

// DestroyKeepaliveAgents closes every pooled socket so none outlives quit
func DestroyKeepaliveAgents() {
	jsonTransport.CloseIdleConnections()
	downloadTransport.CloseIdleConnections()
}

var transientCodes = map[string]bool{
	"ECONNRESET": true, "ECONNREFUSED": true, "EPIPE": true, "ETIMEDOUT": true,
	"EAI_AGAIN": true, "ENOTFOUND": true, "EHOSTUNREACH": true, "ENETUNREACH": true,
}

var neverSentCodes = map[string]bool{
	"ECONNREFUSED": true, "ENOTFOUND": true, "EAI_AGAIN": true, "EHOSTUNREACH": true, "ENETUNREACH": true,
}

// NodeNetError is a transport failure carrying the Node error code and message the renderer classifies
type NodeNetError struct {
	Code    string
	Message string
	Err     error
}

func (e *NodeNetError) Error() string { return e.Message }
func (e *NodeNetError) Unwrap() error { return e.Err }

// ErrorCode is the Node style code of the failure
func (e *NodeNetError) ErrorCode() string { return e.Code }

// Node code of a transport failure, the wrapped Go error mapped when it carries none
func nodeErrorCode(err error) string {
	var n *NodeNetError
	if errors.As(err, &n) && n.Code != "" {
		return n.Code
	}
	if errors.Is(err, syscall.EPIPE) {
		return "EPIPE"
	}
	return errorCode(err)
}

// Node style transport error for a failed exchange with host, nil passes through
func nodeTransportError(err error, host string) error {
	if err == nil {
		return nil
	}
	var already *NodeNetError
	if errors.As(err, &already) {
		return err
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		code := "EAI_AGAIN"
		if dns.IsNotFound {
			code = "ENOTFOUND"
		}
		return &NodeNetError{Code: code, Message: "getaddrinfo " + code + " " + dns.Name, Err: err}
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		addr := host
		if op.Addr != nil {
			addr = op.Addr.String()
		}
		for _, pair := range []struct {
			errno syscall.Errno
			code  string
		}{{syscall.ECONNREFUSED, "ECONNREFUSED"}, {syscall.ETIMEDOUT, "ETIMEDOUT"}, {syscall.EHOSTUNREACH, "EHOSTUNREACH"}, {syscall.ENETUNREACH, "ENETUNREACH"}, {syscall.ECONNRESET, "ECONNRESET"}} {
			if errors.Is(err, pair.errno) {
				return &NodeNetError{Code: pair.code, Message: "connect " + pair.code + " " + addr, Err: err}
			}
		}
		if op.Timeout() {
			return &NodeNetError{Code: "ETIMEDOUT", Message: "connect ETIMEDOUT " + addr, Err: err}
		}
	}
	switch {
	case errors.Is(err, syscall.ECONNRESET):
		return &NodeNetError{Code: "ECONNRESET", Message: "read ECONNRESET", Err: err}
	case errors.Is(err, syscall.EPIPE):
		return &NodeNetError{Code: "EPIPE", Message: "write EPIPE", Err: err}
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return &NodeNetError{Code: "ECONNRESET", Message: "socket hang up", Err: err}
	}
	if tlsErr := nodeTLSError(err, host); tlsErr != nil {
		return tlsErr
	}
	return err
}

// Node TLS error text and code for a certificate failure, nil when err is not one
func nodeTLSError(err error, host string) error {
	var unknown x509.UnknownAuthorityError
	if errors.As(err, &unknown) {
		if unknown.Cert != nil && unknown.Cert.Subject.String() == unknown.Cert.Issuer.String() {
			return &NodeNetError{Code: "DEPTH_ZERO_SELF_SIGNED_CERT", Message: "self-signed certificate", Err: err}
		}
		return &NodeNetError{Code: "UNABLE_TO_VERIFY_LEAF_SIGNATURE", Message: "unable to verify the first certificate", Err: err}
	}
	var hostname x509.HostnameError
	if errors.As(err, &hostname) {
		names := []string{}
		if hostname.Certificate != nil {
			for _, name := range hostname.Certificate.DNSNames {
				names = append(names, "DNS:"+name)
			}
			for _, ip := range hostname.Certificate.IPAddresses {
				names = append(names, "IP Address:"+ip.String())
			}
		}
		return &NodeNetError{Code: "ERR_TLS_CERT_ALTNAME_INVALID", Message: "Hostname/IP does not match certificate's altnames: Host: " + host + ". is not in the cert's altnames: " + strings.Join(names, ", "), Err: err}
	}
	var invalid x509.CertificateInvalidError
	if errors.As(err, &invalid) && invalid.Reason == x509.Expired {
		return &NodeNetError{Code: "CERT_HAS_EXPIRED", Message: "certificate has expired", Err: err}
	}
	return nil
}

// IsIdempotentMethod reports GET, HEAD and OPTIONS, an empty verb counting as GET
func IsIdempotentMethod(method string) bool {
	switch strings.ToUpper(orGet(method)) {
	case "GET", "HEAD", "OPTIONS":
		return true
	}
	return false
}

func orGet(method string) string {
	if method == "" {
		return "GET"
	}
	return method
}

// IsTransientTransportError reports a transport error a retry may survive
func IsTransientTransportError(err error) bool {
	if err == nil {
		return false
	}
	if transientCodes[nodeErrorCode(err)] {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "socket hang up") || strings.Contains(msg, "read ECONNRESET")
}

// RequestState records whether an attempt started putting its request on the wire
type RequestState struct{ BodySent bool }

// ShouldRetryRequest retries idempotent verbs on transient errors and others only when nothing reached the server
func ShouldRetryRequest(err error, method string, state RequestState) bool {
	if !IsTransientTransportError(err) {
		return false
	}
	if IsIdempotentMethod(method) {
		return true
	}
	if neverSentCodes[nodeErrorCode(err)] {
		return true
	}
	return !state.BodySent
}

// RetryPolicy bounds WithRetry, a negative MaxRetries meaning the default of two
type RetryPolicy struct {
	Method     string
	MaxRetries int
	Delay      func(attempt int)
}

// DefaultRetryPolicy is two retries for the verb with the doubling 200 ms backoff capped at 2 s
func DefaultRetryPolicy(method string) RetryPolicy {
	return RetryPolicy{Method: method, MaxRetries: -1}
}

// WithRetry runs attempt under the verb gated retry policy, each attempt with a fresh request state
func WithRetry[T any](attempt func(state *RequestState) (T, error), policy RetryPolicy) (T, error) {
	method := strings.ToUpper(orGet(policy.Method))
	maxRetries := policy.MaxRetries
	if maxRetries < 0 {
		maxRetries = 2
	}
	delay := policy.Delay
	if delay == nil {
		delay = func(n int) {
			time.Sleep(time.Duration(math.Min(200*math.Pow(2, float64(n)), 2000)) * time.Millisecond)
		}
	}
	var zero T
	var last error
	for n := 0; n <= maxRetries; n++ {
		state := &RequestState{}
		out, err := attempt(state)
		if err == nil {
			return out, nil
		}
		last = err
		if n < maxRetries && ShouldRetryRequest(err, method, *state) {
			delay(n)
			continue
		}
		return zero, err
	}
	return zero, last
}

// HTTPStatusError is the failure of an HTTP status of 400 or more, message status colon body
func HTTPStatusError(statusCode int, text, statusMessage string) error {
	status := statusCode
	if status <= 0 {
		status = 500
	}
	detail := text
	if detail == "" {
		detail = statusMessage
	}
	return &backend.HTTPError{StatusCode: status, Message: strconv.Itoa(status) + ": " + detail}
}

// ReadStatusCode is the HTTP status an error carries, zero when it carries none
func ReadStatusCode(err error) int {
	status, _ := readStatusCode(err)
	return status
}

// ReadJSONErrorBody is the JSON object after the status prefix of an HTTP failure, nil when absent
func ReadJSONErrorBody(err error) map[string]any {
	if err == nil {
		return nil
	}
	message := err.Error()
	start := strings.IndexByte(message, '{')
	if start < 0 {
		return nil
	}
	var parsed any
	if json.Unmarshal([]byte(message[start:]), &parsed) != nil {
		return nil
	}
	obj, _ := parsed.(map[string]any)
	return obj
}

// HTMLResponseError explains a JSON endpoint that redirected or answered HTML
func HTMLResponseError(rawURL string, statusCode int, location string) error {
	if statusCode >= 300 && statusCode < 400 {
		target := strings.TrimSpace(location)
		hint := "This is usually an authentication proxy in front of the gateway; check the saved token and extra gateway headers."
		if isSchemeOrSlashRedirect(rawURL, target) {
			hint = "The saved gateway URL differs from the server by scheme or trailing slash; update it to match."
		}
		to := ""
		if target != "" {
			to = " to " + target
		}
		return fmt.Errorf("Expected JSON from %s but the request was redirected (status %d)%s. %s", rawURL, statusCode, to, hint)
	}
	return fmt.Errorf("Expected JSON from %s but got HTML (status %d). The endpoint is likely missing on the Icebath backend.", rawURL, statusCode)
}

func isSchemeOrSlashRedirect(requestURL, location string) bool {
	if location == "" {
		return false
	}
	from, err := parseURL(requestURL)
	if err != nil {
		return false
	}
	to, err := parseURLRef(location, requestURL)
	if err != nil {
		return false
	}
	strip := func(v string) string { return strings.TrimRight(v, "/") }
	return from.Host() == to.Host() && from.Search() == to.Search() &&
		(from.Protocol() != to.Protocol() || from.Pathname() != to.Pathname()) &&
		strip(from.Pathname()) == strip(to.Pathname())
}
