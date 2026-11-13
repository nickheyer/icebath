package connections

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// LoginIdentity names the connection a pre-save sign-in writes its cookies for
type LoginIdentity struct {
	ConnectionID    string
	PendingAuthMode string
	PendingKind     string
}

// Partition a cookie request or sign-in for url rides, the legacy jar when the registry cannot be read
func (s *Service) resolveOauthPartitionForURL(url string, id LoginIdentity) string {
	s.mu.Lock()
	registry, err := s.readDesktopConnectionsRegistry()
	config := s.readDesktopConnectionConfig()
	s.mu.Unlock()
	if err != nil {
		return LegacyOauthPartition
	}
	return ResolveOauthPartition(url, ResolveOauthPartitionOptions{
		Registry: &registry, V1RemoteURL: jsOrString(jsObject(config.Remote)["url"]),
		ConnectionID: id.ConnectionID, PendingAuthMode: id.PendingAuthMode, PendingKind: id.PendingKind,
	})
}

// Loads the partition jar of url from disk before a read trusts a negative
func (s *Service) warmOauthCookieStore(url string) {
	s.partitionJar(s.resolveOauthPartitionForURL(url, LoginIdentity{})).All()
}

// Copies the jar's cookies for the gateway into the explicit Cookie header mirror
func (s *Service) captureRemoteSessionCookiesIntoMirror(partition, baseURL string) {
	s.auth.state().cookies.RecordFromJar(partition, baseURL, s.partitionJar(partition).Get(baseURL))
}

// Cookies of a gateway in a partition jar, by URL and then by host
func (s *Service) gatewayCookies(partition, baseURL string) []Cookie {
	jar := s.partitionJar(partition)
	cookies := jar.Get(baseURL)
	if _, ok := parseJarURL(baseURL); !ok {
		parsed, err := parseURL(baseURL)
		if err != nil {
			return nil
		}
		cookies = jar.GetDomain(parsed.Hostname())
	}
	out := make([]Cookie, len(cookies))
	for i, c := range cookies {
		out[i] = Cookie{Name: c.Name, Value: c.Value}
	}
	return out
}

// Whether the identity's jar holds a live access token cookie for the gateway
func (s *Service) hasOauthSessionCookie(baseURL string, id LoginIdentity) bool {
	return CookiesHaveSession(s.gatewayCookies(s.resolveOauthPartitionForURL(baseURL, id), baseURL))
}

// Whether an access or refresh cookie is held for the gateway, rereading before trusting a negative
func (s *Service) hasLiveOauthSession(baseURL string) bool {
	partition := s.resolveOauthPartitionForURL(baseURL, LoginIdentity{})
	readLive := func() bool { return CookiesHaveLiveSession(s.gatewayCookies(partition, baseURL)) }
	if readLive() {
		return true
	}
	s.warmOauthCookieStore(baseURL)
	for _, delay := range []time.Duration{30, 60, 90} {
		if readLive() {
			return true
		}
		time.Sleep(delay * time.Millisecond)
	}
	return readLive()
}

// Removes every cookie the gateway's jar holds for its URL
func (s *Service) clearOauthSession(baseURL string) {
	jar := s.partitionJar(s.resolveOauthPartitionForURL(baseURL, LoginIdentity{}))
	cookies := jar.All()
	if baseURL != "" {
		cookies = jar.Get(baseURL)
	}
	for _, c := range cookies {
		scheme := "http"
		if c.Secure {
			scheme = "https"
		}
		path := c.Path
		if path == "" {
			path = "/"
		}
		jar.Remove(scheme+"://"+c.Domain+path, c.Name)
	}
}

// LoginWindowOptions shape one gateway sign-in window
type LoginWindowOptions struct {
	Silent     bool
	Background bool
	Identity   LoginIdentity
}

// Opens a gateway sign-in window on the identity's jar, one flow per jar and origin
func (s *Service) openOauthLoginWindow(ctx context.Context, baseURL string, o LoginWindowOptions) (OauthLoginOutcome, error) {
	unattended := o.Background || !CanShowInteractiveOauthLogin(ctx)
	partition := s.resolveOauthPartitionForURL(baseURL, o.Identity)
	return s.auth.state().flights.Run(LoginFlightKey(partition, baseURL), !unattended, func() (LoginAttempt, error) {
		host := s.authHost()
		if host == nil || !host.CanOpenWindows() {
			return LoginAttempt{}, errors.New("Desktop is not ready to start an OAuth login.")
		}
		normalized, err := NormalizeRemoteBaseURL(baseURL)
		if err != nil {
			return LoginAttempt{}, err
		}
		jar := s.partitionJar(partition)
		var created atomic.Pointer[LoginWindow]
		spec := LoginWindowSpec{Partition: partition, Width: 520, Height: 720, Cookies: jar.All(), Headers: NewRemoteHeaderInjector(s.headersForRemoteRequest)}
		loginRendererLifecycle(&spec, "oauth", s.log, func() bool {
			win := created.Load()
			return win != nil && (*win).IsDestroyed()
		})
		return StartOauthLoginWindow(normalized, o.Silent, unattended, OauthLoginWindowDeps{
			Spec: spec,
			CreateWindow: func(spec LoginWindowSpec) (LoginWindow, error) {
				win, err := host.OpenLoginWindow(spec)
				if err == nil {
					created.Store(&win)
				}
				return win, err
			},
			HasSessionCookie: func(win LoginWindow) bool {
				if cookies, err := win.Cookies(); err == nil {
					jar.ReplaceAll(cookies)
				}
				return s.hasOauthSessionCookie(baseURL, o.Identity)
			},
			LoadHeadersFor: func(loginURL string) map[string]string {
				headers := s.headersForRemoteRequest(loginURL)
				hostName := loginURL
				if parsed, err := parseURL(loginURL); err == nil {
					hostName = parsed.Host()
				}
				s.log(fmt.Sprintf("OAuth login: attaching %d extra gateway header(s) to %s", len(headers), hostName))
				return headers
			},
			Harvest:  jar.ReplaceAll,
			OnLanded: func() { s.captureRemoteSessionCookiesIntoMirror(partition, baseURL) },
			Log:      s.log,
		}), nil
	})
}

var errTooManyRedirects = errors.New("net::ERR_TOO_MANY_REDIRECTS")

// ChromiumNetError is a failed Electron net request, carrying the Chromium net error text and no Node code
type ChromiumNetError struct{ Message string }

func (e *ChromiumNetError) Error() string { return e.Message }

// Chromium net error of a failed partition exchange
func chromiumNetError(err error) error {
	if errors.Is(err, errTooManyRedirects) {
		return &ChromiumNetError{Message: errTooManyRedirects.Error()}
	}
	var tlsErr *NodeNetError
	if errors.As(nodeTLSError(err, ""), &tlsErr) {
		switch tlsErr.Code {
		case "CERT_HAS_EXPIRED":
			return &ChromiumNetError{Message: "net::ERR_CERT_DATE_INVALID"}
		case "ERR_TLS_CERT_ALTNAME_INVALID":
			return &ChromiumNetError{Message: "net::ERR_CERT_COMMON_NAME_INVALID"}
		}
		return &ChromiumNetError{Message: "net::ERR_CERT_AUTHORITY_INVALID"}
	}
	switch nodeErrorCode(nodeTransportError(err, "")) {
	case "ECONNREFUSED":
		return &ChromiumNetError{Message: "net::ERR_CONNECTION_REFUSED"}
	case "ENOTFOUND", "EAI_AGAIN":
		return &ChromiumNetError{Message: "net::ERR_NAME_NOT_RESOLVED"}
	case "ETIMEDOUT":
		return &ChromiumNetError{Message: "net::ERR_CONNECTION_TIMED_OUT"}
	case "ECONNRESET", "EPIPE":
		return &ChromiumNetError{Message: "net::ERR_CONNECTION_RESET"}
	case "EHOSTUNREACH":
		return &ChromiumNetError{Message: "net::ERR_ADDRESS_UNREACHABLE"}
	case "ENETUNREACH":
		return &ChromiumNetError{Message: "net::ERR_INTERNET_DISCONNECTED"}
	}
	return &ChromiumNetError{Message: "net::ERR_FAILED"}
}

// One exchange over the partition jar that follows redirects, keeps every hop's cookies and caps the whole call
func (s *Service) oauthSessionExchange(ctx context.Context, partition, rawURL string, o JSONOptions) (any, error) {
	parsed, err := parseURL(rawURL)
	if err != nil {
		return nil, errors.New("Invalid URL: Invalid URL")
	}
	if parsed.Protocol() != "http:" && parsed.Protocol() != "https:" {
		return nil, errors.New("Unsupported Icebath backend URL protocol: " + parsed.Protocol())
	}
	var body []byte
	if o.HasBody {
		if body, err = jsonBody(o.Body); err != nil {
			return nil, err
		}
	}
	timeoutMs := resolveTimeoutMs(o.TimeoutMs, DefaultFetchTimeoutMs)
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	jar := s.partitionJar(partition)
	mirror := s.auth.state().cookies
	injector := NewRemoteHeaderInjector(s.headersForRemoteRequest)
	requestID := 1
	headers := spreadHeaders(map[string]string{"Content-Type": "application/json"}, s.headersForRemoteRequest(rawURL), o.Headers)
	explicitCookie := headers.Get("Cookie") != ""
	if !explicitCookie {
		if mirrored := mirror.CookieHeaderFor(partition, rawURL); mirrored != "" {
			headers.Set("Cookie", mirrored)
			explicitCookie = true
		}
	}
	apply := func(req *http.Request) {
		current := map[string]string{}
		for name := range req.Header {
			current[name] = req.Header.Get(name)
		}
		if out := injector.Apply(RemoteRequestDetails{ID: &requestID, URL: req.URL.String(), RequestHeaders: current}); out != nil {
			req.Header = spreadHeaders(out)
		}
		if !explicitCookie {
			req.Header.Del("Cookie")
			if cookie := jar.CookieHeader(req.URL.String()); cookie != "" {
				req.Header.Set("Cookie", cookie)
			}
		}
	}
	client := &http.Client{Transport: jsonTransport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 20 {
			return errTooManyRedirects
		}
		if req.Response != nil {
			jar.SetFromResponse(via[len(via)-1].URL.String(), req.Response.Header)
		}
		apply(req)
		return nil
	}}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, orGet(o.Method), parsed.Href(false), reader)
	if err != nil {
		return nil, err
	}
	req.Header = headers
	apply(req)
	defer injector.Forget(&requestID)
	timedOut := fmt.Errorf("Timed out connecting to Icebath backend after %dms", timeoutMs)
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, timedOut
		}
		return nil, chromiumNetError(err)
	}
	defer resp.Body.Close()
	finalURL := resp.Request.URL.String()
	jar.SetFromResponse(finalURL, resp.Header)
	mirror.Record(partition, rawURL, resp.Header.Values("Set-Cookie"))
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, timedOut
		}
		return nil, chromiumNetError(err)
	}
	text := strings.ToValidUTF8(string(raw), "�")
	status := resp.StatusCode
	if status >= 400 {
		return nil, HTTPStatusError(status, text, "")
	}
	return decodeJSONText(rawURL, status, text, resp.Header.Get("Content-Type"), func() error {
		return fmt.Errorf("Expected JSON from %s but got HTML (status %d).", rawURL, status)
	})
}

// JSON call riding the OAuth partition with the mirrored cookies, one silent re-login and retry on a gate 401
func (s *Service) fetchJSONViaOauthSession(ctx context.Context, rawURL string, o JSONOptions) (any, error) {
	partition := s.resolveOauthPartitionForURL(rawURL, LoginIdentity{})
	attempt := func() (any, error) { return s.oauthSessionExchange(ctx, partition, rawURL, o) }
	out, err := attempt()
	if err == nil {
		return out, nil
	}
	origin := OriginKeyFor(rawURL)
	if parsed, perr := parseURL(rawURL); perr == nil {
		origin = urlOrigin(parsed.Protocol(), parsed.Host())
	}
	flights := s.auth.state().flights
	return RetryCookie401WithLogin(ctx, err, o.Method, o.ReplayOn401,
		func() {
			if !flights.IsRunning(LoginFlightKey(partition, origin)) {
				s.auth.state().cookies.Clear(partition, rawURL)
			}
		},
		func() error {
			_, loginErr := s.openOauthLoginWindow(ctx, origin, LoginWindowOptions{Silent: true, Background: true})
			if loginErr != nil {
				hostName := rawURL
				if parsed, perr := parseURL(rawURL); perr == nil {
					hostName = parsed.Host()
				}
				s.log(fmt.Sprintf("Remote session re-login after 401 failed for %s: %s", hostName, loginErr.Error()))
			}
			return loginErr
		},
		attempt)
}
