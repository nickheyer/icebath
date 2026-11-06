package verify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Connection whose every read and write gets the five second socket timeout urlopen sets
type timeoutConn struct {
	net.Conn
	timeout time.Duration
}

func (c *timeoutConn) Read(p []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	return c.Conn.Read(p)
}

func (c *timeoutConn) Write(p []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(c.timeout))
	return c.Conn.Write(p)
}

// urlopen client, environment proxies, a socket timeout per operation and the urllib redirect limits
func urlopenClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		Proxy:             pyurl.UrlopenProxy,
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, &dialError{err}
			}
			return &timeoutConn{Conn: conn, timeout: timeout}, nil
		},
	}
	return &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		target := req.URL.String()
		visits, distinct := 0, map[string]bool{}
		for _, prior := range via[1:] {
			distinct[prior.URL.String()] = true
			if prior.URL.String() == target {
				visits++
			}
		}
		if visits >= 4 || len(distinct) >= 10 {
			return http.ErrUseLastResponse
		}
		return nil
	}}
}

// Failure to connect, the URLError urlopen wraps around the socket error
type dialError struct{ err error }

func (e *dialError) Error() string { return e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

// RequestError is the InvalidURL or UnicodeEncodeError http.client raises before sending a readiness request
type RequestError struct {
	Class string
	Msg   string
}

func (e *RequestError) Error() string { return e.Msg }

// http.client refusals of a readiness URL, a nonnumeric port or a path with control or non ASCII characters
func validateReadinessURL(rawURL string) error {
	rest := strings.TrimPrefix(rawURL, "http://")
	hostport, path := rest, "/"
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		hostport, path = rest[:i], rest[i:]
	}
	if i := strings.LastIndexByte(hostport, ':'); i >= 0 {
		if port := hostport[i+1:]; port != "" && strings.Trim(port, "0123456789") != "" {
			return &RequestError{Class: "InvalidURL", Msg: "nonnumeric port: '" + port + "'"}
		}
	}
	if i := strings.IndexByte(path, '#'); i >= 0 {
		path = path[:i]
	}
	for _, c := range path {
		if c <= 0x20 || c == 0x7f {
			return &RequestError{Class: "InvalidURL", Msg: "URL can't contain control characters. " + pyrepr.Repr(path) + " (found at least " + pyrepr.Repr(string(c)) + ")"}
		}
	}
	pos := len("GET ")
	for _, c := range path {
		if c > 0x7f {
			esc := fmt.Sprintf("\\U%08x", c)
			if c < 0x100 {
				esc = fmt.Sprintf("\\x%02x", c)
			} else if c < 0x10000 {
				esc = fmt.Sprintf("\\u%04x", c)
			}
			return &RequestError{Class: "UnicodeEncodeError", Msg: fmt.Sprintf("'ascii' codec can't encode character '%s' in position %d: ordinal not in range(128)", esc, pos)}
		}
		pos++
	}
	return nil
}

// Polls url until it answers any HTTP status or timeout passes, keeping the last connection error text
func pollReadiness(rawURL string, timeout float64, interval time.Duration) (bool, *int, *string, error) {
	if err := validateReadinessURL(rawURL); err != nil {
		return false, nil, nil, err
	}
	client := urlopenClient(5 * time.Second)
	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	var lastError *string
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			return false, nil, nil, err
		}
		req.Header.Set("User-Agent", "icebath-verify")
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			status := resp.StatusCode
			return true, &status, nil, nil
		}
		text, ok := urlopenErrorText(err)
		if !ok {
			return false, nil, nil, err
		}
		lastError = &text
		time.Sleep(interval)
	}
	return false, nil, lastError, nil
}

// str of the URLError, OSError or TimeoutError urlopen raises, false for an exception the poll lets propagate
func urlopenErrorText(err error) (string, bool) {
	var de *dialError
	if errors.As(err, &de) {
		if isTimeout(de.err) {
			return "<urlopen error timed out>", true
		}
		return (&pyurl.URLError{Reason: pyhttpx.OSErrorText(de.err)}).Error(), true
	}
	switch {
	case isTimeout(err):
		return "timed out", true
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return "Remote end closed connection without response", true
	case errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNABORTED):
		return pyhttpx.OSErrorText(err), true
	}
	return "", false
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, os.ErrDeadlineExceeded) || errors.As(err, &ne) && ne.Timeout()
}
