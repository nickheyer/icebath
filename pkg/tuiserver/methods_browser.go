//go:build !icebath_core && !icebath_noextras

package tuiserver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/netpref"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	syncutil "github.com/nickheyer/icebath/pkg/utils/sync"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

// URL schemes a browser CDP endpoint may use
var cdpSchemes = map[string]bool{"http": true, "https": true, "ws": true, "wss": true}

// Binds the browser card method and the vault release of a closing session
func init() { browserManageHandler = browserManage }

// Tool env of the scoped profile home the browser session reads
func browserEnv(ctx context.Context) *tools.Env {
	return tools.EnvForProfileHome(ctx, nil, home.Dir(ctx))
}

// Status, connect or disconnect of the profile's live browser CDP endpoint
func browserManage(ctx context.Context, rid any, params map[string]any) map[string]any {
	p, err := tuigateway.BrowserManage.Params(params)
	if err != nil {
		return rpcErr(rid, 4015, err.Error(), nil)
	}
	switch p.Action {
	case "", "status":
		cdpURL := resolveBrowserCDPURL(ctx)
		return ok(rid, tuigateway.Plain(tuigateway.BrowserManageResult{Connected: cdpURL != "", URL: cdpURL}))
	case "disconnect":
		return browserDisconnect(ctx, rid)
	}
	return browserConnect(ctx, rid, p)
}

// Configured browser CDP override with no network I/O
func resolveBrowserCDPURL(ctx context.Context) string {
	return browser.CDPOverrideRaw(browserEnv(ctx))
}

// Whether a parsed URL is the discovery style local default, never a concrete websocket endpoint
func isDefaultLocalCDP(parsed *url.URL) bool {
	port, hasPort, err := urlutil.Port(parsed)
	if err != nil {
		return false
	}
	if !hasPort {
		port = 80
	}
	host := strings.ToLower(parsed.Hostname())
	return (parsed.Scheme == "http" || parsed.Scheme == "ws") && (host == "127.0.0.1" || host == "localhost") && port == 9222 &&
		(parsed.Path == "" || parsed.Path == "/" || parsed.Path == "/json" || parsed.Path == "/json/version")
}

// Whether /json/version or /json on the CDP host answers 2xx
func cdpHTTPReachable(parsed *url.URL, timeout time.Duration) bool {
	scheme := map[string]string{"ws": "http", "wss": "https"}[parsed.Scheme]
	if scheme == "" {
		scheme = parsed.Scheme
	}
	root := (&url.URL{Scheme: scheme, User: parsed.User, Host: parsed.Host}).String()
	client := &http.Client{Timeout: timeout, Transport: &http.Transport{DialContext: netpref.DialContext((&net.Dialer{}).DialContext), Proxy: httputil.Proxy}}
	for _, target := range []string{root + "/json/version", root + "/json"} {
		resp, err := client.Get(target)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return true
		}
	}
	return false
}

// Discovers or launches the default local debug browser, announcing every step, empty when none answers
func connectLocalDefault(ctx context.Context, port int, system string, announce func(message string, level tuigateway.BrowserProgressLevel)) string {
	discovered := browser.DiscoverLocalCDPURL(ctx, port, 2*time.Second)
	if discovered != "" {
		announce("Chromium-family browser is already listening at "+discovered, "info")
		return discovered
	}
	launchPort := port
	if browser.LocalPortInUse(port, 500*time.Millisecond) {
		launchPort = browser.FindFreeDebugPort(port, 10)
		announce(fmt.Sprintf("Port %d is occupied by another application that isn't a CDP browser "+
			"(an IDE debugger or dev server may be using it) — launching a debug browser "+
			"on port %d instead...", port, launchPort), "info")
	} else {
		announce("Chromium-family browser isn't running with remote debugging — attempting to launch...", "info")
	}
	launch := browser.LaunchChromeDebug(ctx, launchPort, system)
	if launch.Launched {
		discovered = syncutil.PollUntil(ctx, 10*time.Second, 500*time.Millisecond, func() (string, bool) {
			found := browser.DiscoverLocalCDPURL(ctx, launchPort, time.Second)
			return found, found != ""
		})
	}
	if discovered != "" {
		announce(fmt.Sprintf("Chromium-family browser launched and listening on port %d", launchPort), "info")
		return discovered
	}
	if hint := launch.Hint(); hint != "" {
		announce(hint, "error")
	}
	var hints []string
	if command := browser.ManualChromeDebugCommand(ctx, launchPort, system); command != "" {
		hints = []string{"Start a Chromium-family browser with remote debugging, then retry /browser connect:", command}
	} else {
		hints = []string{"No supported Chromium-family browser executable was found in this environment.",
			fmt.Sprintf("Install one or start a Chromium-family browser with --remote-debugging-port=%d, then retry /browser connect.", launchPort)}
	}
	hints = append(hints, "Browser not connected — start a Chromium-family browser with remote debugging and retry /browser connect")
	for _, line := range hints {
		announce(line, "error")
	}
	return ""
}

// Closes the current profile's browsers only, other profiles keeping theirs
func reapProfileBrowsers(ctx context.Context) error {
	return browser.CleanupProfileBrowsers(ctx, browserEnv(ctx), home.Key(home.Dir(ctx)))
}

// Validates, reaches or launches and then publishes the profile's live CDP endpoint
func browserConnect(ctx context.Context, rid any, p tuigateway.BrowserManageParams) (response map[string]any) {
	cdpURL := ""
	if p.URL != nil {
		cdpURL = strings.TrimSpace(*p.URL)
	}
	if cdpURL == "" {
		cdpURL = browser.DefaultBrowserCDPURL
	}
	sid := p.SessionID
	system := runtime.GOOS
	messages := []string{}
	announce := func(message string, level tuigateway.BrowserProgressLevel) {
		messages = append(messages, message)
		if sid != "" {
			emitEvent(ctx, tuigateway.BrowserProgressEvent.Name(), sid, tuigateway.BrowserProgressEvent.Wire(tuigateway.BrowserProgressPayload{Message: message, Level: level}))
		}
	}
	target := cdpURL
	if !strings.Contains(cdpURL, "://") {
		target = "http://" + cdpURL
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return rpcErr(rid, 4015, fmt.Sprintf("invalid browser url: %s", cdpURL), nil)
	}
	if !cdpSchemes[parsed.Scheme] {
		return rpcErr(rid, 4015, fmt.Sprintf("unsupported browser url: %s", cdpURL), nil)
	}
	if parsed.Hostname() == "" {
		return rpcErr(rid, 4015, fmt.Sprintf("missing host in browser url: %s", cdpURL), nil)
	}
	port, hasPort, err := urlutil.Port(parsed)
	if err != nil {
		return rpcErr(rid, 4015, fmt.Sprintf("invalid port in browser url: %s", cdpURL), nil)
	}
	if !hasPort {
		port = 80
		if parsed.Scheme == "https" || parsed.Scheme == "wss" {
			port = 443
		}
	}
	if isDefaultLocalCDP(parsed) {
		cdpURL = browser.DefaultBrowserCDPURL
		if parsed, err = url.Parse(cdpURL); err != nil {
			panic(err)
		}
		if port, hasPort, _ = urlutil.Port(parsed); !hasPort {
			port = 9222
		}
	}
	defer func() {
		if failure := recover(); failure != nil {
			if configOutage(failure) {
				panic(failure)
			}
			response = rpcErr(rid, 5031, failureText(failure), nil)
		}
	}()
	switch {
	case (parsed.Scheme == "ws" || parsed.Scheme == "wss") && strings.HasPrefix(parsed.Path, "/devtools/browser/"):
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(parsed.Hostname(), strconv.Itoa(port)), 2*time.Second)
		if err != nil {
			return rpcErr(rid, 5031, fmt.Sprintf("could not reach browser CDP at %s: %s", cdpURL, err), nil)
		}
		conn.Close()
	case isDefaultLocalCDP(parsed):
		discovered := connectLocalDefault(ctx, port, system, announce)
		if discovered == "" {
			return ok(rid, tuigateway.Plain(tuigateway.BrowserManageResult{URL: cdpURL, Messages: messages}))
		}
		cdpURL = discovered
		if parsed, err = url.Parse(cdpURL); err != nil {
			panic(err)
		}
	case !cdpHTTPReachable(parsed, 2*time.Second):
		return rpcErr(rid, 5031, fmt.Sprintf("could not reach browser CDP at %s", cdpURL), nil)
	}
	normalized := parsed.String()
	if !strings.HasPrefix(parsed.Path, "/devtools/browser/") {
		bare := *parsed
		bare.Path, bare.RawPath, bare.RawQuery, bare.ForceQuery, bare.Fragment, bare.RawFragment = "", "", "", false, "", ""
		normalized = bare.String()
	}
	if err := reapProfileBrowsers(ctx); err != nil {
		panic(err)
	}
	browser.SetLiveCDPOverride(home.Key(home.Dir(ctx)), normalized)
	if err := reapProfileBrowsers(ctx); err != nil {
		panic(err)
	}
	return ok(rid, tuigateway.Plain(tuigateway.BrowserManageResult{Connected: true, URL: normalized, Messages: messages}))
}

// Drops the profile's live CDP endpoint between two reaps, a failed reap swallowed
func browserDisconnect(ctx context.Context, rid any) map[string]any {
	reap := func() {
		suppress(func() {
			if err := reapProfileBrowsers(ctx); err != nil {
				panic(err)
			}
		})
	}
	reap()
	browser.SetLiveCDPOverride(home.Key(home.Dir(ctx)), "")
	reap()
	return ok(rid, tuigateway.Plain(tuigateway.BrowserManageResult{}))
}
