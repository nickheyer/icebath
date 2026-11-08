package provider

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Header names forwarded to a different origin, every other header is dropped there
var crossOriginSafeHeaders = map[string]bool{"accept": true, "user-agent": true}

var urllibDefaultPorts = map[string]int{"http": 80, "https": 443}

// Origin is the normalized scheme, hostname and effective port of a URL, Port -1 when none applies
type Origin struct {
	Scheme string
	Host   string
	Port   int
}

// String renders the origin as one comparable key
func (o Origin) String() string {
	if o.Port < 0 {
		return o.Scheme + "://" + o.Host
	}
	return o.Scheme + "://" + o.Host + ":" + strconv.Itoa(o.Port)
}

// URLOrigin parses a URL into its origin, erroring on a malformed port so the request fails closed
func URLOrigin(rawURL string) (Origin, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return Origin{}, err
	}
	scheme := strings.ToLower(parsed.Scheme)
	port := -1
	if raw := parsed.Port(); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil {
			return Origin{}, errors.New("Port could not be cast to integer value as " + strconv.Quote(raw))
		}
		if n < 0 || n > 65535 {
			return Origin{}, errors.New("Port out of range 0-65535")
		}
		port = n
	} else if def, ok := urllibDefaultPorts[scheme]; ok {
		port = def
	}
	host := strings.TrimRight(strings.ToLower(parsed.Hostname()), ".")
	return Origin{Scheme: scheme, Host: host, Port: port}, nil
}

// Origin of a URL as one string, empty when the URL does not parse
func urlOrigin(raw string) string {
	origin, err := URLOrigin(raw)
	if err != nil {
		return ""
	}
	return origin.String()
}

// Drops every header whose lowercased name is outside keep
func stripHeaders(header http.Header, keep map[string]bool) {
	for name := range header {
		if !keep[strings.ToLower(name)] {
			delete(header, name)
		}
	}
}

// SafeCredentialRedirectHandler keeps request headers only while redirects stay on one origin
type SafeCredentialRedirectHandler struct {
	originalOrigin         Origin
	crossOriginSafeHeaders map[string]bool
}

// NewSafeCredentialRedirectHandler binds the policy to the first URL's origin and the headers allowed across origins
func NewSafeCredentialRedirectHandler(originalURL string, crossOriginSafe ...string) (*SafeCredentialRedirectHandler, error) {
	origin, err := URLOrigin(originalURL)
	if err != nil {
		return nil, err
	}
	safe := crossOriginSafeHeaders
	if crossOriginSafe != nil {
		safe = map[string]bool{}
		for _, name := range crossOriginSafe {
			safe[strings.ToLower(name)] = true
		}
	}
	return &SafeCredentialRedirectHandler{originalOrigin: origin, crossOriginSafeHeaders: safe}, nil
}

// True when urllib would refuse the chain as an infinite loop
func redirectLoop(req *http.Request, via []*http.Request) bool {
	visited := map[string]int{}
	for _, r := range via[1:] {
		visited[r.URL.String()]++
	}
	return visited[req.URL.String()] >= 4 || len(visited) >= 10
}

// RedirectRequest applies urllib's status and method rules, then carries the previous hop's headers only within the origin
func (h *SafeCredentialRedirectHandler) RedirectRequest(req *http.Request, via []*http.Request) error {
	prev := via[len(via)-1]
	code := 0
	if req.Response != nil {
		code = req.Response.StatusCode
	}
	method := prev.Method
	if method == "" {
		method = http.MethodGet
	}
	readable := method == http.MethodGet || method == http.MethodHead
	allowed := (readable && (code == 301 || code == 302 || code == 303 || code == 307 || code == 308)) ||
		(method == http.MethodPost && (code == 301 || code == 302 || code == 303))
	if !allowed || redirectLoop(req, via) {
		return http.ErrUseLastResponse
	}
	headers := prev.Header.Clone()
	if headers == nil {
		headers = http.Header{}
	}
	headers.Del("Content-Length")
	headers.Del("Content-Type")
	req.Header = headers
	target, err := URLOrigin(req.URL.String())
	if err != nil {
		return err
	}
	if target != h.originalOrigin {
		stripHeaders(req.Header, h.crossOriginSafeHeaders)
	}
	return nil
}

// Redirect policy of the credential safe handler for one request URL, failing closed on a malformed URL
func credentialRedirectPolicy(rawURL string) func(*http.Request, []*http.Request) error {
	handler, err := NewSafeCredentialRedirectHandler(rawURL)
	if err != nil {
		return func(*http.Request, []*http.Request) error { return err }
	}
	return handler.RedirectRequest
}

// Strips unsafe headers after installed request processors ran so none re-adds a secret across origins
type crossOriginRequestSanitizer struct {
	originalOrigin Origin
}

func newCrossOriginRequestSanitizer(originalURL string) (*crossOriginRequestSanitizer, error) {
	origin, err := URLOrigin(originalURL)
	if err != nil {
		return nil, err
	}
	return &crossOriginRequestSanitizer{originalOrigin: origin}, nil
}

// Request with every unsafe header removed when its origin differs from the original one
func (s *crossOriginRequestSanitizer) sanitize(req *http.Request) (*http.Request, error) {
	origin, err := URLOrigin(req.URL.String())
	if err != nil {
		return nil, err
	}
	if origin != s.originalOrigin {
		stripHeaders(req.Header, crossOriginSafeHeaders)
	}
	return req, nil
}

// RequestProcessor rewrites a request before it is sent, urllib's http_request hook
type RequestProcessor func(*http.Request) *http.Request

// Opener is a urllib OpenerDirector in Go, a transport behind default headers, request processors and a redirect policy
type Opener struct {
	Transport         http.RoundTripper
	TLS               *tls.Config
	Processors        []RequestProcessor
	AddHeaders        [][2]string
	Redirect          func(*http.Request, []*http.Request) error
	sanitizer         *crossOriginRequestSanitizer
	initialAddHeaders [][2]string
}

var (
	installedOpenerMu sync.Mutex
	installedOpener   *Opener
)

// InstallOpener makes opener the process wide policy credentialed requests clone, nil uninstalls it
func InstallOpener(opener *Opener) {
	installedOpenerMu.Lock()
	defer installedOpenerMu.Unlock()
	installedOpener = opener
}

func currentInstalledOpener() *Opener {
	installedOpenerMu.Lock()
	defer installedOpenerMu.Unlock()
	return installedOpener
}

// Round tripper of the opener with a timeout bound transport built when none is installed
func (o *Opener) roundTripper(timeout time.Duration) http.RoundTripper {
	inner := o.Transport
	if inner == nil {
		inner = &http.Transport{
			Proxy:                 probeProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: timeout}).DialContext,
			TLSClientConfig:       o.TLS,
			TLSHandshakeTimeout:   timeout,
			ResponseHeaderTimeout: timeout,
		}
	}
	return &openerTransport{inner: inner, opener: o}
}

// Open sends one request through the opener under the timeout
func (o *Opener) Open(req *http.Request, timeout time.Duration) (*http.Response, error) {
	client := &http.Client{Transport: o.roundTripper(timeout), CheckRedirect: o.Redirect, Timeout: timeout}
	return client.Do(req)
}

type openerTransport struct {
	inner  http.RoundTripper
	opener *Opener
}

// Applies unredirected default headers, the processors and the sanitizer to a clone before the inner transport
func (t *openerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cur := req.Clone(req.Context())
	for _, h := range t.opener.AddHeaders {
		if cur.Header.Get(h[0]) == "" {
			cur.Header.Set(h[0], h[1])
		}
	}
	for _, p := range t.opener.Processors {
		cur = p(cur)
	}
	if t.opener.sanitizer != nil {
		var err error
		if cur, err = t.opener.sanitizer.sanitize(cur); err != nil {
			return nil, err
		}
	}
	return t.inner.RoundTrip(cur)
}

// TLS context of process owned openers, nil meaning the platform store the installed trust store verifies against
var resolvedHTTPSContext = func() *tls.Config {
	InstallTrustStore()
	return nil
}

// Installed HTTPS handler rebound to a TLS context, a foreign round tripper kept as the custom protocol handler it is
func transportBoundTo(transport http.RoundTripper, tlsCfg *tls.Config) http.RoundTripper {
	switch tr := transport.(type) {
	case nil:
		return nil
	case *http.Transport:
		cloned := tr.Clone()
		cloned.TLSClientConfig = tlsCfg
		return cloned
	}
	return transport
}

// Clone of an opener with only its redirect policy replaced and default headers moved to the initial request
func secureOpenerFromPolicy(installed *Opener, originalURL string, tlsCfg *tls.Config) (*Opener, error) {
	if installed == nil {
		installed = &Opener{TLS: resolvedHTTPSContext()}
	}
	redirect, err := NewSafeCredentialRedirectHandler(originalURL)
	if err != nil {
		return nil, err
	}
	sanitizer, err := newCrossOriginRequestSanitizer(originalURL)
	if err != nil {
		return nil, err
	}
	secured := &Opener{
		Transport:         installed.Transport,
		TLS:               installed.TLS,
		Processors:        append([]RequestProcessor(nil), installed.Processors...),
		Redirect:          redirect.RedirectRequest,
		sanitizer:         sanitizer,
		initialAddHeaders: append([][2]string(nil), installed.AddHeaders...),
	}
	if tlsCfg != nil {
		secured.TLS = tlsCfg
		secured.Transport = transportBoundTo(installed.Transport, tlsCfg)
	}
	return secured, nil
}

// Secured clone of the installed opener, a process owned opener when none is installed
func secureOpenerFromInstalledPolicy(originalURL string, tlsCfg *tls.Config) (*Opener, error) {
	return secureOpenerFromPolicy(currentInstalledOpener(), originalURL, tlsCfg)
}

// RequestOpener opens one request under a timeout
type RequestOpener interface {
	Open(req *http.Request, timeout time.Duration) (*http.Response, error)
}

// OpenerFactory is the explicit test seam building the opener around the redirect handler
type OpenerFactory func(redirect *SafeCredentialRedirectHandler) RequestOpener

// OpenCredentialedURL opens a request without forwarding credentials across origins
func OpenCredentialedURL(req *http.Request, timeout time.Duration, openerFactory OpenerFactory, tlsCfg *tls.Config) (*http.Response, error) {
	if req.Header == nil {
		req.Header = http.Header{}
	}
	if openerFactory == nil {
		opener, err := secureOpenerFromInstalledPolicy(req.URL.String(), tlsCfg)
		if err != nil {
			return nil, err
		}
		for _, h := range opener.initialAddHeaders {
			if req.Header.Get(h[0]) == "" {
				req.Header.Set(h[0], h[1])
			}
		}
		return opener.Open(req, timeout)
	}
	redirect, err := NewSafeCredentialRedirectHandler(req.URL.String())
	if err != nil {
		return nil, err
	}
	return openerFactory(redirect).Open(req, timeout)
}
