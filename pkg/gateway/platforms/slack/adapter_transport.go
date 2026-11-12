package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// HTTP client for Slack calls with the proxy pinned, direct when proxyURL is empty and HTTP_PROXY ignored
func slackHTTPClient(proxyURL string) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: transport, Timeout: slackWebTimeout}
}

// Request timeout of every Web API client, the 30 second default of slack_sdk's AsyncWebClient
var slackWebTimeout = 30 * time.Second

const socketTaskCancelTimeout = 3 * time.Second

// One Socket Mode goroutine with its cancel function, completion signal and exit error
type socketTask struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// Runs fn on its own goroutine under a cancellable child of ctx, then onDone with the finished task
func startSocketTask(ctx context.Context, fn func(context.Context) error, onDone func(*socketTask)) *socketTask {
	taskCtx, cancel := context.WithCancel(ctx)
	t := &socketTask{cancel: cancel, done: make(chan struct{})}
	go func() {
		t.err = fn(taskCtx)
		close(t.done)
		if onDone != nil {
			go onDone(t)
		}
	}()
	return t
}

// Whether the goroutine has returned
func (t *socketTask) isDone() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// Cancels live Socket Mode tasks and waits for them up to the cancel timeout, logging failures and stragglers
func cancelSocketTasks(tasks []*socketTask) {
	var live []*socketTask
	for _, t := range tasks {
		if t != nil && !t.isDone() {
			live = append(live, t)
		}
	}
	for _, t := range live {
		t.cancel()
	}
	if len(live) == 0 {
		return
	}
	timer := time.NewTimer(socketTaskCancelTimeout)
	defer timer.Stop()
	expired := false
	stillRunning := 0
	for _, t := range live {
		if !expired {
			select {
			case <-t.done:
			case <-timer.C:
				expired = true
			}
		}
		if !t.isDone() {
			stillRunning++
			continue
		}
		if t.err != nil && !errors.Is(t.err, context.Canceled) {
			slog.Debug("[Slack] Socket Mode task failed while stopping", "error", t.err)
		}
	}
	if stillRunning > 0 {
		slog.Warn(fmt.Sprintf("[Slack] %d Socket Mode task(s) did not stop within %.1fs", stillRunning, socketTaskCancelTimeout.Seconds()))
	}
}

// HTTP client honouring proxy environment variables only when the gateway trusts the environment
func trustEnvHTTPClient(ctx context.Context, timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if gateway.GatewayTrustEnv(ctx) {
		transport.Proxy = http.ProxyFromEnvironment
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

var slackProxyHosts = []string{"slack.com", "files.slack.com", "wss-primary.slack.com"}

// Proxy URL Slack clients can use, empty for unsupported schemes or when NO_PROXY covers a Slack host
func resolveSlackProxyURL(ctx context.Context) string {
	proxyURL, err := gateway.ResolveProxyURL(ctx, "", nil, "")
	if err != nil || proxyURL == "" {
		return ""
	}
	normalized := strings.ToLower(proxyURL)
	if !strings.HasPrefix(normalized, "http://") && !strings.HasPrefix(normalized, "https://") {
		slog.Info(fmt.Sprintf("[Slack] Ignoring unsupported proxy scheme for Slack transport: %s", gateway.SafeURLForLog(proxyURL, 80)))
		return ""
	}
	for _, host := range slackProxyHosts {
		if gateway.IsHostExcludedByNoProxy(host, nil) {
			slog.Info("[Slack] NO_PROXY bypasses Slack proxy configuration")
			return ""
		}
	}
	return proxyURL
}
