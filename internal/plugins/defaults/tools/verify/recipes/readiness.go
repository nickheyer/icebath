package recipes

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
)

// Longest one connection, read or write of a readiness request may take
const probeTimeout = 5 * time.Second

// Connection whose every read and write gets the probe's timeout
type timedConn struct {
	net.Conn
}

func (c timedConn) Read(p []byte) (int, error) {
	c.Conn.SetReadDeadline(time.Now().Add(probeTimeout))
	return c.Conn.Read(p)
}

func (c timedConn) Write(p []byte) (int, error) {
	c.Conn.SetWriteDeadline(time.Now().Add(probeTimeout))
	return c.Conn.Write(p)
}

// Failure to connect, set apart from a failure of the exchange after it
type dialError struct{ err error }

func (e *dialError) Error() string { return e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

// Client of the readiness probe: the environment's proxies, a timeout on each step and a few redirects at most, any answer counting
func probeClient() *http.Client {
	dialer := &net.Dialer{Timeout: probeTimeout}
	transport := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, &dialError{err}
			}
			return timedConn{conn}, nil
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

// Why a readiness URL can't be asked: a port that is no number, a path holding a space, a control character or one a request line can't
// carry
func checkURL(url string) error {
	rest := strings.TrimPrefix(url, "http://")
	host, path := rest, "/"
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		host, path = rest[:i], rest[i:]
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		if port := host[i+1:]; port != "" && strings.Trim(port, "0123456789") != "" {
			return fmt.Errorf("readiness URL port %s is not a number", port)
		}
	}
	if i := strings.IndexByte(path, '#'); i >= 0 {
		path = path[:i]
	}
	for i, c := range []rune(path) {
		switch {
		case c <= 0x20 || c == 0x7f:
			return fmt.Errorf("readiness URL path %s holds a space or control character", path)
		case c > 0x7f:
			return fmt.Errorf("readiness URL path holds the non-ASCII character %c at position %d, which a request line cannot carry", c, i)
		}
	}
	return nil
}

// Asks the URL until it answers with any status or the wait runs out: whether it answered, its status, and the last reason it could not
// be reached
func poll(ctx context.Context, url string, wait, interval time.Duration) (bool, *int, string) {
	client := probeClient()
	deadline := time.Now().Add(wait)
	failure := ""
	for time.Now().Before(deadline) && ctx.Err() == nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false, nil, err.Error()
		}
		req.Header.Set("User-Agent", "icebath-verify")
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			status := resp.StatusCode
			return true, &status, ""
		}
		failure = reason(err)
		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}
	return false, nil, failure
}

// Plain reason a readiness request failed
func reason(err error) string {
	if dial, ok := errors.AsType[*dialError](err); ok {
		err = dial.err
	}
	var errno syscall.Errno
	var timeout net.Error
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout():
		return "timed out"
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return "Remote end closed connection without response"
	case errors.As(err, &errno):
		return errno.Error()
	}
	return err.Error()
}
