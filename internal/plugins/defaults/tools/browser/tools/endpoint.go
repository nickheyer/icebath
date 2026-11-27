package tools

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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/tools/redact"
)

// Endpoint the /browser connect of each profile published, and how many times one was
type liveOverrides struct {
	mu          sync.Mutex
	urls        map[string]string
	generations map[string]int
}

func (l *liveOverrides) get(profile string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.urls[profile]
}

// Publishes the profile's live endpoint, empty clearing it, and moves its generation on
func (l *liveOverrides) set(profile, endpoint string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.urls == nil {
		l.urls, l.generations = map[string]string{}, map[string]int{}
	}
	if endpoint != "" {
		l.urls[profile] = endpoint
	} else {
		delete(l.urls, profile)
	}
	l.generations[profile]++
}

func (l *liveOverrides) generation(profile string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.generations[profile]
}

var hostPortOnly = regexp.MustCompile(`^wss?://[^/]+:\d+/?$`)

// HTTP client asking a debugging port, never through a proxy for loopback
var discovery = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: loopbackProxy}}

// Endpoint safe to log, the credentials of its query and userinfo masked, such as a cloud session's signed address
func redactEndpoint(text string) string {
	return redact.Text(text, false, redact.Ends)
}

// Endpoint as a concrete websocket address, found over /json/version for a debugging port or a bare host and port
func resolveCDP(ctx context.Context, endpoint string) string {
	raw := strings.TrimSpace(endpoint)
	if raw == "" {
		return ""
	}
	lowered := strings.ToLower(raw)
	if strings.Contains(lowered, "/devtools/browser/") {
		return raw
	}
	found := raw
	if strings.HasPrefix(lowered, "ws://") || strings.HasPrefix(lowered, "wss://") {
		if !hostPortOnly.MatchString(raw) {
			return raw
		}
		scheme := "http://"
		if strings.HasPrefix(lowered, "wss://") {
			scheme = "https://"
		}
		_, rest, _ := strings.Cut(raw, "://")
		found = scheme + rest
	}
	version := found
	if !strings.HasSuffix(strings.ToLower(found), "/json/version") {
		version = strings.TrimRight(found, "/") + "/json/version"
	}
	ws, err := webSocketURL(ctx, version)
	if err != nil {
		slog.Warn("browser: CDP discovery failed, using raw endpoint", "endpoint", redactEndpoint(raw), "error", redactEndpoint(err.Error()))
		return raw
	}
	if ws == "" {
		slog.Warn("browser: CDP discovery returned no webSocketDebuggerUrl", "endpoint", redactEndpoint(version))
		return raw
	}
	return ws
}

// webSocketDebuggerUrl of a /json/version answer
func webSocketURL(ctx context.Context, version string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, version, nil)
	if err != nil {
		return "", err
	}
	resp, err := discovery.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d from %s", resp.StatusCode, version)
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

// Whether a TCP connection to the host and port opens in time
func tcpOpen(host string, port int, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(port)), timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Whether the address exposes a DevTools endpoint that answers
func debugReady(ctx context.Context, raw string, timeout time.Duration) bool {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	port := 80
	if scheme == "https" || scheme == "wss" {
		port = 443
	}
	if p := u.Port(); p != "" {
		if port, err = strconv.Atoi(p); err != nil {
			return false
		}
	}
	if (scheme == "ws" || scheme == "wss") && strings.HasPrefix(u.Path, "/devtools/browser/") {
		return tcpOpen(u.Hostname(), port, timeout)
	}
	httpScheme := map[string]string{"ws": "http", "wss": "https", "http": "http", "https": "https"}[scheme]
	if httpScheme == "" {
		return false
	}
	root := httpScheme + "://" + u.Host
	client := &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: loopbackProxy}}
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

// First loopback address on the port that speaks CDP, IPv4 first since a squatter on one can push Chrome onto the other; empty for none
func discoverLocal(ctx context.Context, port int, timeout time.Duration) string {
	for _, host := range []string{"127.0.0.1", "[::1]"} {
		u := fmt.Sprintf("http://%s:%d", host, port)
		if debugReady(ctx, u, timeout) {
			return u
		}
	}
	return ""
}

// Whether either loopback takes TCP on the port
func portInUse(port int, timeout time.Duration) bool {
	return tcpOpen("127.0.0.1", port, timeout) || tcpOpen("::1", port, timeout)
}
