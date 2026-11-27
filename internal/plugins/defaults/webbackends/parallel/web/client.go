package web

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/http/httpguts"

	"github.com/nickheyer/icebath/internal/config"
	parallelconfig "github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/parallel/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/web"
	"github.com/nickheyer/icebath/pkg/httpx"
)

const (
	// Beta the search and extract endpoints need
	beta = "search-extract-2025-10-10"
	// Times a failed call is tried again, and the bounds of the wait before each, in seconds
	retries      = 2
	initialDelay = 0.5
	maxDelay     = 8.0
	// Longest a connection may take to open, and the server to answer or to send more of its answer
	connectLimit = 5 * time.Second
	idleLimit    = 600 * time.Second
)

// Answer whose body stopped arriving for longer than the idle limit
var errStalled = errors.New("the answer stopped arriving")

// Client with the limits of Parallel's SDK, a redirect to another host losing the key
func newClient() *http.Client {
	dialer := &net.Dialer{Timeout: connectLimit, KeepAlive: 30 * time.Second}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   connectLimit,
			ResponseHeaderTimeout: idleLimit,
			MaxIdleConns:          32,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if req.URL.Host != via[0].URL.Host {
				req.Header.Del("x-api-key")
			}
			return nil
		},
	}
}

// JSON answer of the API to a call, tried as the SDK tries it: a connection failure or a status worth retrying is tried twice more,
// after the wait the server asks for or a growing backoff. Async marks the SDK client the call stands for, the async one for page reads
func (b backends) post(ctx context.Context, cfg *config.Config, token, path string, payload any, async bool) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	target := endpoint(cmp.Or(strings.TrimSpace(cfg.GetString(parallelconfig.BaseURL)), parallelconfig.DefaultBaseURL), path)
	custom := extra(cfg)
	for retry := 0; ; retry++ {
		response, err := b.send(ctx, target, body, headers(token, custom, async, retry))
		var wait time.Duration
		switch left := retry < retries; {
		case err != nil && ctx.Err() != nil:
			return nil, ctx.Err()
		case err != nil && left:
			wait = delay(retry, nil)
		case err != nil && timedOut(err):
			return nil, errors.New("Request timed out.")
		case err != nil:
			return nil, errors.New("Connection error.")
		case response.Status >= 400 && left && retryable(response):
			wait = delay(retry, response.Header)
		case response.Status >= 400:
			return nil, refused(response)
		default:
			return reply(response)
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// One attempt, its whole answer read, the idle limit starting again with every part of the body that arrives
func (b backends) send(ctx context.Context, target string, body []byte, header http.Header) (*web.Response, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = header
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var stalled atomic.Bool
	timer := time.AfterFunc(idleLimit, func() {
		stalled.Store(true)
		cancel()
	})
	defer timer.Stop()
	var data bytes.Buffer
	chunk := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(chunk)
		if n > 0 {
			data.Write(chunk[:n])
			timer.Reset(idleLimit)
		}
		switch {
		case err == io.EOF:
			return &web.Response{Status: resp.StatusCode, Header: resp.Header, Body: data.Bytes(), URL: resp.Request.URL.String()}, nil
		case err != nil && stalled.Load():
			return nil, errStalled
		case err != nil:
			return nil, err
		}
	}
}

// Whether an attempt failed for running out of time
func timedOut(err error) bool {
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return true
	}
	return errors.Is(err, errStalled)
}

func sleep(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Address of an API path under the base, which is read as a folder
func endpoint(base, path string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base + path
	}
	return u.JoinPath(path).String()
}

// Headers of an attempt: those of the SDK, the key, the extra ones set, then the beta, the count of tries before and the read limit
func headers(token string, custom http.Header, async bool, retry int) http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", provider.UserAgent("icebath"))
	h.Set("X-Stainless-Lang", "go")
	h.Set("X-Stainless-Package-Version", provider.Version())
	h.Set("X-Stainless-OS", system())
	h.Set("X-Stainless-Arch", architecture())
	h.Set("X-Stainless-Runtime", "go")
	h.Set("X-Stainless-Runtime-Version", runtime.Version())
	h.Set("x-api-key", token)
	h.Set("X-Stainless-Async", "false")
	if async {
		h.Set("X-Stainless-Async", "async:goroutine")
	}
	for name, values := range custom {
		h[name] = values
	}
	h.Set("parallel-beta", beta)
	h.Set("x-stainless-retry-count", strconv.Itoa(retry))
	h.Set("x-stainless-read-timeout", strconv.Itoa(int(idleLimit/time.Second)))
	return h
}

// Extra headers set, each Name: value: the items of the setting, or the lines of the text its variable holds. A line that names no header
// that can be sent is left out
func extra(cfg *config.Config) http.Header {
	lines := cfg.GetStringSlice(parallelconfig.Headers)
	if text, ok := cfg.Get(parallelconfig.Headers).(string); ok {
		lines = strings.Split(text, "\n")
	}
	h := http.Header{}
	for _, line := range lines {
		name, value, ok := strings.Cut(line, ":")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if ok && httpguts.ValidHeaderFieldName(name) && httpguts.ValidHeaderFieldValue(value) {
			h.Set(name, value)
		}
	}
	return h
}

// Operating system as the SDK names it
func system() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	case "android":
		return "Android"
	case "ios":
		return "iOS"
	case "freebsd":
		return "FreeBSD"
	case "openbsd":
		return "OpenBSD"
	case "linux":
		return "Linux"
	}
	return "Other:" + runtime.GOOS
}

// Processor architecture as the SDK names it
func architecture() string {
	switch runtime.GOARCH {
	case "arm64", "arm":
		return runtime.GOARCH
	case "amd64":
		return "x64"
	case "386":
		return "x32"
	}
	return "other:" + runtime.GOARCH
}

// Wait before the next try: the one the server asks for when it is under a minute, else a backoff doubling from half a second up to
// eight, less up to a quarter at random
func delay(retry int, header http.Header) time.Duration {
	if after, ok := retryAfter(header); ok && after > 0 && after <= 60 {
		return time.Duration(after * float64(time.Second))
	}
	seconds := min(initialDelay*math.Pow(2, float64(retry)), maxDelay) * (1 - 0.25*rand.Float64())
	return time.Duration(max(seconds, 0) * float64(time.Second))
}

// Seconds the server asks to wait, in retry-after-ms, or in retry-after as seconds or an HTTP date
func retryAfter(header http.Header) (float64, bool) {
	if ms, err := strconv.ParseFloat(strings.TrimSpace(header.Get("retry-after-ms")), 64); err == nil {
		return ms / 1000, true
	}
	after, ok := httpx.RetryAfter(header.Get("retry-after"))
	return after.Seconds(), ok
}

// Whether a failed status is tried again: as x-should-retry says, else for a timeout, a conflict, a rate limit or a server error
func retryable(response *web.Response) bool {
	return httpx.Retried(response.Status, response.Header)
}

// Failure of a status the call is not tried again for: the status then a JSON body as sent, a JSON string as its text; any other body
// alone; the status alone for an empty body
func refused(response *web.Response) error {
	text := strings.TrimSpace(response.Text())
	var value any
	if json.Unmarshal([]byte(text), &value) == nil {
		if s, ok := value.(string); ok {
			text = s
		}
		return fmt.Errorf("Error code: %d - %s", response.Status, text)
	}
	if text != "" {
		return errors.New(text)
	}
	return fmt.Errorf("Error code: %d", response.Status)
}

// Body of a successful answer as the SDK reads it: JSON, told as it is when the answer says JSON and is not, else the text as a JSON string
// when it is not JSON
func reply(response *web.Response) ([]byte, error) {
	contentType := response.Header.Get("Content-Type")
	body := web.Decoded(contentType, response.Body)
	media, _, _ := strings.Cut(cmp.Or(contentType, "*"), ";")
	err := json.Unmarshal(body, new(json.RawMessage))
	switch {
	case err == nil:
		return body, nil
	case strings.HasSuffix(media, "json"):
		return nil, web.Verbatim(err.Error())
	}
	return json.Marshal(response.Text())
}
