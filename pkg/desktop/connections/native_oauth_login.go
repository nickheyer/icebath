package connections

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/hostinfo"
)

// DefaultLoginTimeout is how long a native login waits for the loopback callback
const DefaultLoginTimeout = 5 * time.Minute

const nativeLoginDoneHTML = `<!doctype html><meta charset="utf-8"><title>Signed in</title>` +
	`<body style="font:15px system-ui;margin:3rem;text-align:center">` +
	`<h2>&#10003; Signed in to Icebath</h2>` +
	`<p>You can close this window and return to the app.</p>` +
	`<script>setTimeout(()=>window.close(),800)</script>`

var callbackQuery = regexp.MustCompile(`[?&](code|error)=`)

// NativeLoginDeps are the browser open and the cookieless token exchange of a native login
type NativeLoginDeps struct {
	OpenExternal func(url string) error
	PostJSON     func(url string, body any, timeoutMs float64) (any, error)
	RememberLog  func(line string)
}

// RunNativeLogin runs the system browser, loopback and PKCE login and exchanges the code for tokens
func RunNativeLogin(baseURL string, deps NativeLoginDeps) (NativeTokenSet, error) {
	log := deps.RememberLog
	if log == nil {
		log = func(string) {}
	}
	pkce := GeneratePkcePair()
	state := GenerateState()
	type outcome struct {
		tokens NativeTokenSet
		err    error
	}
	done := make(chan outcome, 1)
	var once sync.Once
	settled := false
	var mu sync.Mutex
	settle := func(o outcome) {
		once.Do(func() { done <- o })
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return NativeTokenSet{}, err
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(nativeLoginDoneHTML))
		mu.Lock()
		if settled {
			mu.Unlock()
			return
		}
		requestURL := r.RequestURI
		if requestURL == "" {
			requestURL = "/"
		}
		if !callbackQuery.MatchString(requestURL) {
			mu.Unlock()
			return
		}
		settled = true
		mu.Unlock()
		code, err := ParseLoopbackCallback(requestURL, state)
		if err != nil {
			settle(outcome{err: err})
			return
		}
		go func() {
			tokenURL, err := NativeTokenURL(baseURL)
			if err != nil {
				settle(outcome{err: err})
				return
			}
			body, err := deps.PostJSON(tokenURL, map[string]any{"code": code, "code_verifier": pkce.Verifier}, 15_000)
			if err != nil {
				settle(outcome{err: err})
				return
			}
			tokens, err := ParseTokenResponse(body)
			settle(outcome{tokens: tokens, err: err})
		}()
	})}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			settle(outcome{err: err})
		}
	}()
	defer server.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := "http://127.0.0.1:" + strconv.Itoa(port) + "/callback"
	authorizeURL, err := BuildNativeAuthorizeURL(baseURL, pkce.Challenge, redirectURI, state, "")
	if err != nil {
		return NativeTokenSet{}, err
	}
	timer := time.NewTimer(DefaultLoginTimeout)
	defer timer.Stop()
	log(fmt.Sprintf("[native-oauth] loopback listening on 127.0.0.1:%d; opening system browser", port))
	go func() {
		if err := deps.OpenExternal(authorizeURL); err != nil {
			settle(outcome{err: errors.New("Could not open the system browser for native sign-in: " + err.Error())})
		}
	}()
	select {
	case o := <-done:
		return o.tokens, o.err
	case <-timer.C:
		mu.Lock()
		settled = true
		mu.Unlock()
		return NativeTokenSet{}, errors.New("Native sign-in timed out. The browser window may not have completed " +
			"sign-in; open Settings → Gateway and try again.")
	}
}

// ExternalOpenResult is the outcome of opening a URL outside the app
type ExternalOpenResult struct {
	OK      bool
	Reason  string
	Message string
}

// ExternalBrowser is the system browser and the open failure modal the shell provides
type ExternalBrowser interface {
	OpenURL(url string) error
	NotifyOpenFailed(url, message string)
}

var webProtocols = map[string]bool{"http:": true, "https:": true, "mailto:": true}

// Opens a web URL in the system browser, through cmd.exe start under WSL, raising the failure modal on error
func (s *Service) openExternalWebURL(rawURL string) ExternalOpenResult {
	raw := strings.TrimSpace(rawURL)
	parsed, err := parseURL(raw)
	if raw == "" || err != nil || !webProtocols[parsed.Protocol()] {
		return ExternalOpenResult{Reason: "invalid"}
	}
	target := parsed.Href(false)
	browser := s.authHost()
	fail := func(err error) ExternalOpenResult {
		message := err.Error()
		s.log("[link] openExternal failed: " + message)
		browser.NotifyOpenFailed(target, message)
		return ExternalOpenResult{Reason: "failed", Message: message}
	}
	if hostinfo.IsWSL() {
		s.log("[link] opening via WSL→Windows: " + target)
		cmd := exec.Command("cmd.exe", "/c", "start", `""`, target)
		detachProcess(cmd)
		if err := cmd.Start(); err != nil {
			s.log("[link] cmd.exe start failed: " + err.Error() + "; falling back to xdg-open")
			go func() {
				if err := browser.OpenURL(target); err != nil {
					fail(err)
				}
			}()
			return ExternalOpenResult{OK: true}
		}
		go func() { _ = cmd.Wait() }()
		return ExternalOpenResult{OK: true}
	}
	if err := browser.OpenURL(target); err != nil {
		return fail(err)
	}
	return ExternalOpenResult{OK: true}
}

// Native login through the single external open path, failing fast when the browser cannot open
func (s *Service) runNativeLogin(ctx context.Context, baseURL string) (NativeTokenSet, error) {
	return RunNativeLogin(baseURL, NativeLoginDeps{
		OpenExternal: func(url string) error {
			result := s.openExternalWebURL(url)
			if result.OK {
				return nil
			}
			if result.Reason == "failed" && result.Message != "" {
				return errors.New(result.Message)
			}
			return errors.New("Could not open the system browser for native sign-in")
		},
		PostJSON: func(url string, body any, timeoutMs float64) (any, error) {
			return s.postJSONNoAuth(ctx, url, body, JSONOptions{TimeoutMs: timeoutMs})
		},
		RememberLog: s.log,
	})
}
