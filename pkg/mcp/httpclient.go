package mcp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Characters of a rejection body kept for diagnostics
const httpRejectionBodyChars = 300

// HTTPRejection remembers the last 4xx or 5xx an owned client saw
type HTTPRejection struct {
	Status int
	Method string
	URL    string
	Body   string
}

// Describe renders the rejection the way connect errors quote it
func (r *HTTPRejection) Describe() string {
	if r == nil || r.Status == 0 {
		return ""
	}
	detail := fmt.Sprintf("HTTP %d from %s %s", r.Status, r.Method, r.URL)
	if r.Body != "" {
		detail += ": " + r.Body
	}
	return detail
}

// rejectionRecorder stores the most recent rejection under a lock
type rejectionRecorder struct {
	mu   sync.Mutex
	last HTTPRejection
}

// record keeps status, method, URL and the head of a non stream body
func (r *rejectionRecorder) record(resp *http.Response, body []byte) {
	if resp == nil || resp.StatusCode < 400 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	head := strings.Join(strings.Fields(string(body)), " ")
	if len(head) > httpRejectionBodyChars {
		head = head[:httpRejectionBodyChars]
	}
	r.last = HTTPRejection{Status: resp.StatusCode, Method: resp.Request.Method, URL: resp.Request.URL.String(), Body: head}
}

// snapshot returns a copy of the last rejection
func (r *rejectionRecorder) snapshot() HTTPRejection {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// Authenticator decorates outbound requests with credentials and reacts to an unauthorized answer
type Authenticator interface {
	// Prepare adds credentials to req, acquiring them when none are cached
	Prepare(ctx context.Context, req *http.Request) error
	// HandleUnauthorized reacts to a 401 or 403, true when the request should be retried
	HandleUnauthorized(ctx context.Context, req *http.Request, resp *http.Response) (bool, error)
}

// HTTPOptions tune one owned HTTP client
type HTTPOptions struct {
	URL                   string
	Headers               map[string]string
	ConfiguredHeaderNames map[string]bool
	StrictRedirectHeaders bool
	SSLVerify             any
	ClientCert            any
	ClientKey             any
	ConnectTimeout        time.Duration
	ReadTimeout           time.Duration
	Auth                  Authenticator
	ServerName            string
}

// ownedClient is the HTTP client every remote transport and probe shares the construction of
type ownedClient struct {
	client   *http.Client
	origin   origin
	opts     HTTPOptions
	rejected *rejectionRecorder
}

// origin identifies scheme, host and port for the redirect credential boundary
type origin struct {
	scheme string
	host   string
	port   string
}

// originOf derives the comparison origin of a URL with default ports made explicit
func originOf(u *url.URL) origin {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return origin{scheme: strings.ToLower(u.Scheme), host: strings.ToLower(u.Hostname()), port: port}
}

// newOwnedClient builds a client with TLS, proxy mounts, the redirect boundary and the rejection recorder
func newOwnedClient(opts HTTPOptions) (*ownedClient, error) {
	target, err := url.Parse(opts.URL)
	if err != nil {
		return nil, err
	}
	tlsConf, err := buildTLSConfig(opts.ServerName, opts.SSLVerify, opts.ClientCert, opts.ClientKey)
	if err != nil {
		return nil, err
	}
	connect := opts.ConnectTimeout
	if connect <= 0 {
		connect = 30 * time.Second
	}
	mounts, err := mcpProxyMounts(opts.URL)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy:                 func(req *http.Request) (*url.URL, error) { return mounts[req.URL.Scheme], nil },
		DialContext:           (&net.Dialer{Timeout: connect, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       tlsConf,
		TLSHandshakeTimeout:   connect,
		ResponseHeaderTimeout: readTimeoutOr(opts.ReadTimeout),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		IdleConnTimeout:       90 * time.Second,
	}
	oc := &ownedClient{origin: originOf(target), opts: opts, rejected: &rejectionRecorder{}}
	oc.client = &http.Client{Transport: &bodyCapTransport{inner: transport, limit: MaxHTTPBodyBytes}, CheckRedirect: oc.checkRedirect}
	return oc, nil
}

// readTimeoutOr returns the configured read timeout or the 300 second default
func readTimeoutOr(d time.Duration) time.Duration {
	if d <= 0 {
		return 300 * time.Second
	}
	return d
}

// checkRedirect strips Authorization and, under strict mode, every configured header on a cross origin hop
func (oc *ownedClient) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if originOf(req.URL) != oc.origin {
		req.Header.Del("Authorization")
		if oc.opts.StrictRedirectHeaders {
			for name := range oc.opts.ConfiguredHeaderNames {
				req.Header.Del(name)
			}
		}
	}
	return nil
}

// buildTLSConfig applies ssl_verify and the mTLS client certificate shapes the config accepts
func buildTLSConfig(server string, verify any, cert any, key any) (*tls.Config, error) {
	conf := &tls.Config{MinVersion: tls.VersionTLS12}
	switch v := verify.(type) {
	case nil:
	case bool:
		conf.InsecureSkipVerify = !v
	case string:
		path := expandUser(strings.TrimSpace(v))
		pem, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("MCP server '%s': ssl_verify CA bundle not readable at %q: %w", server, path, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("MCP server '%s': ssl_verify CA bundle at %q holds no certificates", server, path)
		}
		conf.RootCAs = pool
	default:
		return nil, fmt.Errorf("MCP server '%s': ssl_verify must be a boolean or a CA bundle path", server)
	}
	certificate, err := resolveClientCert(server, cert, key)
	if err != nil {
		return nil, err
	}
	if certificate != nil {
		conf.Certificates = []tls.Certificate{*certificate}
	}
	return conf, nil
}

// expandUser replaces a leading tilde with the user's home directory
func expandUser(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return home + path[1:]
		}
	}
	return path
}

// drainClose discards a body and closes it so the connection can be reused
func drainClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<20))
	_ = body.Close()
}

// contentTypeBase lowers the response content type and drops parameters
func contentTypeBase(resp *http.Response) string {
	return baseMime(resp.Header.Get("Content-Type"))
}

// Content types a real MCP endpoint answers with
var mcpContentTypes = map[string]bool{"application/json": true, "text/event-stream": true}

// is2xx reports a success status
func is2xx(resp *http.Response) bool { return resp.StatusCode >= 200 && resp.StatusCode < 300 }
