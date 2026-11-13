package connections

import (
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"
)

// LoginWindowSpec is a native sign-in window over one partition's webview storage
type LoginWindowSpec struct {
	Partition string
	Title     string
	Show      bool
	Width     int
	Height    int
	// Cookies seeded into the window storage before the first load, replacing what it held
	Cookies []JarCookie
	// Header rule applied to every request of the window, redirect hops included
	Headers         *RemoteHeaderInjector
	OnNavigate      func(url string)
	OnRedirect      func(url string)
	OnFrameNavigate func(url string)
	// OnClosed receives every cookie the storage held when the user closed the window
	OnClosed       func(final []JarCookie)
	OnRendererGone func(reason, exitCode string)
	OnUnresponsive func()
	OnFailLoad     func(code, url string)
}

// LoginWindow is the shell's sign-in window
type LoginWindow interface {
	// Load navigates with extra headers on the first request, done receiving the load outcome
	Load(url string, headers map[string]string, done func(err error))
	Show()
	Focus()
	IsVisible() bool
	IsDestroyed() bool
	Destroy()
	// Cookies is every cookie the window storage holds, HttpOnly ones included
	Cookies() ([]JarCookie, error)
}

// LoginLoadError is a failed sign-in load with its Chromium style network error code
type LoginLoadError struct {
	Code    int
	Message string
}

func (e *LoginLoadError) Error() string { return e.Message }

var abortedLoad = regexp.MustCompile(`\bERR_ABORTED\b|\(-3\)`)

// IsExpectedOauthNavigationAbort reports the aborted load a redirect away from the login page causes
func IsExpectedOauthNavigationAbort(err error) bool {
	if err == nil {
		return false
	}
	var load *LoginLoadError
	if errors.As(err, &load) && load.Code == -3 {
		return true
	}
	return abortedLoad.MatchString(err.Error())
}

const signInRequired = "Silent sign-in did not complete; sign-in required."

// OauthLoginWindowDeps are the window factory and the session checks a login window runs over
type OauthLoginWindowDeps struct {
	CreateWindow     func(spec LoginWindowSpec) (LoginWindow, error)
	HasSessionCookie func(win LoginWindow) bool
	LoadHeadersFor   func(loginURL string) map[string]string
	Harvest          func(cookies []JarCookie)
	OnLanded         func()
	Spec             LoginWindowSpec
	Log              func(string)
}

const (
	loginPoll        = 750 * time.Millisecond
	loginRevealAfter = 2500 * time.Millisecond
	loginDeadline    = 12 * time.Second
)

// StartOauthLoginWindow opens a sign-in window that settles once the session cookie lands or the window closes
func StartOauthLoginWindow(baseURL string, silent, background bool, deps OauthLoginWindowDeps) LoginAttempt {
	done := make(chan struct{})
	var mu sync.Mutex
	hiddenRecovery := background
	settled := false
	var win LoginWindow
	var outcome OauthLoginOutcome
	var failure error
	var stopTimers []func() bool
	finish := func(err error) {
		mu.Lock()
		if settled {
			mu.Unlock()
			return
		}
		settled = true
		for _, stop := range stopTimers {
			stop()
		}
		live := win
		mu.Unlock()
		if live != nil && !live.IsDestroyed() {
			if cookies, err := live.Cookies(); err == nil {
				deps.Harvest(cookies)
			}
			live.Destroy()
		}
		if err == nil {
			deps.OnLanded()
		}
		if err != nil {
			failure = err
		} else {
			outcome = OauthLoginOutcome{BaseURL: baseURL, OK: true}
		}
		close(done)
	}
	isSettled := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return settled
	}
	checkCookie := func() {
		mu.Lock()
		live := win
		mu.Unlock()
		if !isSettled() && live != nil && deps.HasSessionCookie(live) {
			finish(nil)
		}
	}
	attempt := LoginAttempt{Done: done, Result: func() (OauthLoginOutcome, error) { return outcome, failure }}
	reveal := func() {}
	attempt.Reveal = func() { reveal() }
	title := "Sign in to Icebath gateway"
	if silent {
		title = "Connecting to Icebath Cloud agent…"
	}
	base := strings.TrimRight(baseURL, "/")
	chooserURL := base + "/login"
	spec := deps.Spec
	spec.Title = title
	spec.Show = !silent && !hiddenRecovery
	spec.OnNavigate = func(url string) {
		mu.Lock()
		hidden := hiddenRecovery
		live := win
		mu.Unlock()
		if hidden && strings.SplitN(strings.SplitN(url, "?", 2)[0], "#", 2)[0] == chooserURL {
			go func() {
				present := live != nil && deps.HasSessionCookie(live)
				mu.Lock()
				stillHidden := hiddenRecovery
				mu.Unlock()
				if present || stillHidden {
					if present {
						finish(nil)
					} else {
						finish(errors.New(signInRequired))
					}
				}
			}()
			return
		}
		go checkCookie()
	}
	spec.OnRedirect = func(string) { go checkCookie() }
	spec.OnFrameNavigate = func(string) { go checkCookie() }
	spec.OnClosed = func(final []JarCookie) {
		if final != nil && !isSettled() {
			deps.Harvest(final)
		}
		finish(errors.New("Login window closed before authentication completed."))
	}
	created, err := deps.CreateWindow(spec)
	if err != nil {
		finish(err)
		return attempt
	}
	mu.Lock()
	win = created
	poll := time.NewTicker(loginPoll)
	stopPoll := make(chan struct{})
	stopTimers = append(stopTimers, func() bool { poll.Stop(); close(stopPoll); return true })
	go func() {
		for {
			select {
			case <-poll.C:
				checkCookie()
			case <-stopPoll:
				return
			}
		}
	}()
	if silent && !hiddenRecovery {
		revealTimer := time.AfterFunc(loginRevealAfter, func() {
			if !isSettled() && !created.IsDestroyed() && !created.IsVisible() {
				created.Show()
			}
		})
		stopTimers = append(stopTimers, revealTimer.Stop)
	}
	var deadline *time.Timer
	if hiddenRecovery {
		deadline = time.AfterFunc(loginDeadline, func() { finish(errors.New(signInRequired)) })
		stopTimers = append(stopTimers, func() bool { return deadline.Stop() })
	}
	reveal = func() {
		mu.Lock()
		if settled || created.IsDestroyed() {
			mu.Unlock()
			return
		}
		hiddenRecovery = false
		if deadline != nil {
			deadline.Stop()
		}
		mu.Unlock()
		created.Show()
		created.Focus()
	}
	mu.Unlock()
	loginURL := chooserURL
	if silent {
		loginURL = base + "/"
	}
	created.Load(loginURL, deps.LoadHeadersFor(loginURL), func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		hidden := hiddenRecovery
		mu.Unlock()
		if hidden && IsExpectedOauthNavigationAbort(err) {
			go checkCookie()
			return
		}
		finish(err)
	})
	return attempt
}

// Renderer lifecycle log lines of a sign-in window of kind, which never reloads
func loginRendererLifecycle(spec *LoginWindowSpec, kind string, log func(string), destroyed func() bool) {
	tag := "[renderer:" + kind + "] "
	spec.OnRendererGone = func(reason, exitCode string) {
		gone := destroyed()
		shownReason, shownCode := reason, exitCode
		if shownReason == "" {
			shownReason = "?"
		}
		if shownCode == "" {
			shownCode = "?"
		}
		teardown := ""
		if gone && reason == "killed" {
			teardown = " (expected teardown)"
		}
		log(tag + "render-process-gone reason=" + shownReason + " exitCode=" + shownCode + teardown)
		if gone || reason == "crashed" || reason == "oom" || reason == "killed" {
			return
		}
		detail := reason
		if detail == "" {
			detail = "unknown"
		}
		if exitCode != "" {
			detail += ", exitCode=" + exitCode
		}
		log(tag + "renderer terminated while live (reason=" + detail + "); not reloading")
	}
	spec.OnUnresponsive = func() { log(tag + "webContents became unresponsive") }
	spec.OnFailLoad = func(code, url string) {
		if code == "" {
			code = "?"
		}
		if url == "" {
			url = "?"
		}
		log(tag + "did-fail-load code=" + code + " url=" + url)
	}
}
