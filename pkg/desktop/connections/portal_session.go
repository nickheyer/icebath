package connections

import (
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

var (
	portalAccessCookieNames  = []string{"nas-session", "__Host-privy-token", "__Secure-privy-token", "privy-token"}
	portalRefreshCookieNames = []string{"nas-refresh", "privy-session", "privy-refresh-token"}
)

// PortalAccessCookies keeps the valued portal access token cookies
func PortalAccessCookies(cookies []JarCookie) []JarCookie {
	out := []JarCookie{}
	for _, c := range cookies {
		if c.Value != "" && slices.Contains(portalAccessCookieNames, c.Name) {
			out = append(out, c)
		}
	}
	return out
}

// CookiesHavePortalAccessToken reports a valued portal access cookie
func CookiesHavePortalAccessToken(cookies []JarCookie) bool {
	return len(PortalAccessCookies(cookies)) > 0
}

// CookiesHavePortalSession reports a portal access or refresh cookie with a value
func CookiesHavePortalSession(cookies []JarCookie) bool {
	if CookiesHavePortalAccessToken(cookies) {
		return true
	}
	for _, c := range cookies {
		if c.Value != "" && slices.Contains(portalRefreshCookieNames, c.Name) {
			return true
		}
	}
	return false
}

// ResolvePortalBaseURL is the Nous portal origin from the environment, the public portal by default
func ResolvePortalBaseURL() string {
	raw := os.Getenv("ICEBATH_PORTAL_BASE_URL")
	if raw == "" {
		raw = os.Getenv("NOUS_PORTAL_BASE_URL")
	}
	if raw == "" {
		raw = "https://portal.nousresearch.com"
	}
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

// Portal cookies of the shared OAuth jar, by URL and then by host
func (s *Service) readPortalCookies() []JarCookie {
	jar := s.partitionJar(LegacyOauthPartition)
	portal := ResolvePortalBaseURL()
	if _, ok := parseJarURL(portal); ok {
		return jar.Get(portal)
	}
	parsed, err := parseURL(portal)
	if err != nil {
		return []JarCookie{}
	}
	return jar.GetDomain(parsed.Hostname())
}

// Whether a portal session cookie is held, rereading after a short backoff before trusting a negative
func (s *Service) hasLivePortalSession() bool {
	if CookiesHavePortalSession(s.readPortalCookies()) {
		return true
	}
	s.warmOauthCookieStore("")
	for _, delay := range []time.Duration{30, 60, 90} {
		if CookiesHavePortalSession(s.readPortalCookies()) {
			return true
		}
		time.Sleep(delay * time.Millisecond)
	}
	return CookiesHavePortalSession(s.readPortalCookies())
}

func (s *Service) hasPortalAccessToken() bool {
	return CookiesHavePortalAccessToken(s.readPortalCookies())
}

type cookieWindowOptions struct {
	kind     string
	title    string
	show     bool
	poll     time.Duration
	deadline time.Duration
}

var errPortalWindowClosed = errors.New("closed")
var errPortalWindowTimeout = errors.New("timeout")

// Drives a portal window on the shared jar until a new access cookie lands, it closes or the deadline passes
func (s *Service) driveCookieWindow(previous []JarCookie, o cookieWindowOptions) error {
	host := s.authHost()
	portal := ResolvePortalBaseURL()
	jar := s.partitionJar(LegacyOauthPartition)
	done := make(chan error, 1)
	var once sync.Once
	var mu sync.Mutex
	var win LoginWindow
	finish := func(outcome error) {
		once.Do(func() {
			mu.Lock()
			live := win
			mu.Unlock()
			if live != nil && !live.IsDestroyed() {
				if cookies, err := live.Cookies(); err == nil {
					jar.ReplaceAll(cookies)
				}
				live.Destroy()
			}
			done <- outcome
		})
	}
	hasNewAccessCookie := func() bool {
		mu.Lock()
		live := win
		mu.Unlock()
		if live == nil {
			return false
		}
		if cookies, err := live.Cookies(); err == nil {
			jar.ReplaceAll(cookies)
		}
		for _, c := range PortalAccessCookies(s.readPortalCookies()) {
			if !slices.ContainsFunc(previous, func(old JarCookie) bool { return old.Name == c.Name && old.Value == c.Value }) {
				return true
			}
		}
		return false
	}
	check := func() {
		if hasNewAccessCookie() {
			finish(nil)
		}
	}
	spec := LoginWindowSpec{Partition: LegacyOauthPartition, Title: o.title, Show: o.show, Width: 520, Height: 720,
		Cookies: jar.All(), Headers: NewRemoteHeaderInjector(s.headersForRemoteRequest),
		OnNavigate: func(string) { go check() }, OnRedirect: func(string) { go check() }, OnFrameNavigate: func(string) { go check() },
		OnClosed: func(final []JarCookie) {
			if final != nil {
				jar.ReplaceAll(final)
			}
			finish(errPortalWindowClosed)
		}}
	loginRendererLifecycle(&spec, o.kind, s.log, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return win != nil && win.IsDestroyed()
	})
	created, err := host.OpenLoginWindow(spec)
	if err != nil {
		return err
	}
	mu.Lock()
	win = created
	mu.Unlock()
	ticker := time.NewTicker(o.poll)
	stop := make(chan struct{})
	defer func() { ticker.Stop(); close(stop) }()
	go func() {
		for {
			select {
			case <-ticker.C:
				check()
			case <-stop:
				return
			}
		}
	}()
	if o.deadline > 0 {
		timer := time.AfterFunc(o.deadline, func() { finish(errPortalWindowTimeout) })
		defer timer.Stop()
	}
	created.Load(portal, nil, func(err error) {
		if err == nil {
			return
		}
		if IsExpectedOauthNavigationAbort(err) {
			go check()
			return
		}
		finish(err)
	})
	return <-done
}

type portalRenewal struct {
	mu      sync.Mutex
	flight  chan struct{}
	outcome bool
}

// Renews the portal access cookie in a hidden window while the refresh session lives, one renewal at a time
func (s *Service) renewPortalAccessSilently(force bool) bool {
	host := s.authHost()
	if host == nil || !host.CanOpenWindows() {
		return false
	}
	if !s.hasLivePortalSession() {
		return false
	}
	if !force && s.hasPortalAccessToken() {
		return true
	}
	renewal := &s.auth.state().portal
	renewal.mu.Lock()
	if flight := renewal.flight; flight != nil {
		renewal.mu.Unlock()
		<-flight
		renewal.mu.Lock()
		defer renewal.mu.Unlock()
		return renewal.outcome
	}
	flight := make(chan struct{})
	renewal.flight = flight
	renewal.mu.Unlock()
	previous := PortalAccessCookies(s.readPortalCookies())
	outcome := s.driveCookieWindow(previous, cookieWindowOptions{kind: "portal-renew", title: "Renewing Icebath Cloud session…", poll: 500 * time.Millisecond, deadline: 12 * time.Second})
	ok := outcome == nil
	verdict := "did not complete"
	if ok {
		verdict = "succeeded"
	}
	s.log("[cloud] silent portal access renewal " + verdict)
	renewal.mu.Lock()
	renewal.outcome = ok
	renewal.flight = nil
	renewal.mu.Unlock()
	close(flight)
	return ok
}

// Interactive portal sign-in on the shared jar, failing unless a new access cookie lands
func (s *Service) openPortalLoginWindow() error {
	host := s.authHost()
	if host == nil || !host.CanOpenWindows() {
		return errors.New("Desktop is not ready to start a Icebath Cloud sign-in.")
	}
	outcome := s.driveCookieWindow(PortalAccessCookies(s.readPortalCookies()), cookieWindowOptions{kind: "portal", title: "Sign in to Icebath Cloud", show: true, poll: 750 * time.Millisecond})
	if outcome == nil {
		return nil
	}
	if outcome == errPortalWindowClosed || outcome == errPortalWindowTimeout {
		return errors.New("Sign-in window closed before authentication completed.")
	}
	return outcome
}
