package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Live /browser connect endpoints and publish counts per home key
var (
	liveOverrideMu   sync.Mutex
	liveOverrides    = map[string]string{}
	liveGenerations  = map[string]int{}
	wsHostPortOnlyRE = regexp.MustCompile(`^wss?://[^/]+:\d+/?$`)
)

// HTTP client for CDP discovery that never routes loopback through a proxy
var discoveryClient = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: loopbackAwareProxy}}

// Proxy from the environment for every host except loopback
func loopbackAwareProxy(req *http.Request) (*url.URL, error) {
	if isLoopbackHost(req.URL.Hostname()) {
		return nil, nil
	}
	return http.ProxyFromEnvironment(req)
}

// ResolveCDPOverride normalizes a CDP endpoint into a concrete websocket URL through /json/version discovery
func ResolveCDPOverride(ctx context.Context, cdpURL string) string {
	raw := strings.TrimSpace(cdpURL)
	if raw == "" {
		return ""
	}
	lowered := strings.ToLower(raw)
	if strings.Contains(lowered, "/devtools/browser/") {
		return raw
	}
	discovery := raw
	if strings.HasPrefix(lowered, "ws://") || strings.HasPrefix(lowered, "wss://") {
		if !wsHostPortOnlyRE.MatchString(raw) {
			return raw
		}
		scheme := "http://"
		if strings.HasPrefix(lowered, "wss://") {
			scheme = "https://"
		}
		discovery = scheme + strings.SplitN(raw, "://", 2)[1]
	}
	versionURL := discovery
	if !strings.HasSuffix(strings.ToLower(discovery), "/json/version") {
		versionURL = strings.TrimRight(discovery, "/") + "/json/version"
	}
	ws, err := discoverWebSocketURL(ctx, versionURL)
	if err != nil {
		slog.Default().Warn("browser: CDP discovery failed, using raw endpoint", "endpoint", tools.RedactCDPURL(raw), "error", tools.RedactCDPURL(err.Error()))
		return raw
	}
	if ws == "" {
		slog.Default().Warn("browser: CDP discovery returned no webSocketDebuggerUrl", "endpoint", tools.RedactCDPURL(versionURL))
		return raw
	}
	return ws
}

// Fetches /json/version and returns its webSocketDebuggerUrl
func discoverWebSocketURL(ctx context.Context, versionURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, versionURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := discoveryClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d from %s", resp.StatusCode, versionURL)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var payload struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	return strings.TrimSpace(payload.WebSocketDebuggerURL), nil
}

// LiveCDPOverride is the home's /browser connect endpoint, empty when none is published
func LiveCDPOverride(homeKey string) string {
	liveOverrideMu.Lock()
	defer liveOverrideMu.Unlock()
	return liveOverrides[homeKey]
}

// SetLiveCDPOverride publishes or with an empty url clears the home's live endpoint and bumps its generation
func SetLiveCDPOverride(homeKey, cdpURL string) {
	liveOverrideMu.Lock()
	defer liveOverrideMu.Unlock()
	if cdpURL != "" {
		liveOverrides[homeKey] = cdpURL
	} else {
		delete(liveOverrides, homeKey)
	}
	liveGenerations[homeKey]++
}

// LiveCDPGeneration counts the home's live endpoint publishes
func LiveCDPGeneration(homeKey string) int {
	liveOverrideMu.Lock()
	defer liveOverrideMu.Unlock()
	return liveGenerations[homeKey]
}

// Reports whether a websocket URL names a browser endpoint rather than a page
func isBrowserEndpoint(wsURL string) bool {
	return strings.Contains(strings.ToLower(wsURL), "/devtools/browser/")
}

// Reports whether a TCP connection to host:port opens within the timeout
func tcpOpen(host string, port int, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(strings.Trim(host, "[]"), fmt.Sprint(port)), timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// IsBrowserDebugReady reports whether a URL exposes a reachable DevTools endpoint
func IsBrowserDebugReady(ctx context.Context, rawURL string, timeout time.Duration) bool {
	if !strings.Contains(rawURL, "://") {
		rawURL = "http://" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	port := 80
	if scheme == "https" || scheme == "wss" {
		port = 443
	}
	if p := u.Port(); p != "" {
		if _, err := fmt.Sscanf(p, "%d", &port); err != nil {
			return false
		}
	}
	if (scheme == "ws" || scheme == "wss") && strings.HasPrefix(u.Path, "/devtools/browser/") {
		return tcpOpen(u.Hostname(), port, timeout)
	}
	httpScheme := map[string]string{"ws": "http", "wss": "https"}[scheme]
	if httpScheme == "" {
		httpScheme = scheme
	}
	if httpScheme != "http" && httpScheme != "https" {
		return false
	}
	root := strings.TrimRight(httpScheme+"://"+u.Host, "/")
	client := &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: loopbackAwareProxy}}
	for _, probe := range []string{root + "/json/version", root + "/json"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, probe, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
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

// Loopback literals probed IPv4 first because a squatter on one can push Chrome onto the other
var loopbackProbeHosts = []string{"127.0.0.1", "[::1]"}

// DiscoverLocalCDPURL returns the first loopback URL on the port speaking CDP, empty when none does
func DiscoverLocalCDPURL(ctx context.Context, port int, timeout time.Duration) string {
	for _, host := range loopbackProbeHosts {
		u := fmt.Sprintf("http://%s:%d", host, port)
		if IsBrowserDebugReady(ctx, u, timeout) {
			return u
		}
	}
	return ""
}

// LocalPortInUse reports whether either loopback accepts TCP on the port
func LocalPortInUse(port int, timeout time.Duration) bool {
	for _, host := range []string{"127.0.0.1", "::1"} {
		if tcpOpen(host, port, timeout) {
			return true
		}
	}
	return false
}
