package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	xunicode "golang.org/x/text/encoding/unicode"
)

const (
	DefaultTokenFetchTimeout = 3 * time.Second
	fetchRedirectLimit       = 20
)

// Fetches a public page as UTF-8 text, refusing schemes other than http and https and any non 2xx answer
var fetchPublicText = func(rawURL string, timeout time.Duration) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" {
		return "", errors.New("Invalid URL")
	}
	if protocol := strings.ToLower(u.Scheme) + ":"; protocol != "http:" && protocol != "https:" {
		return "", fmt.Errorf("Unsupported Icebath backend URL protocol: %s", protocol)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", errors.New("Invalid URL")
	}
	client := &http.Client{CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) > fetchRedirectLimit {
			return errors.New("redirect count exceeded")
		}
		return nil
	}}
	res, err := client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("Timed out connecting to Icebath backend after %dms", timeout.Milliseconds())
		}
		return "", errors.New("fetch failed")
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", errors.New("The operation was aborted due to timeout")
		}
		return "", errors.New("terminated")
	}
	decoded, _ := xunicode.UTF8BOM.NewDecoder().Bytes(raw)
	text := string(decoded)
	if res.StatusCode < 200 || res.StatusCode > 299 {
		detail := text
		if detail == "" {
			detail = strings.TrimSpace(strings.TrimPrefix(res.Status, strconv.Itoa(res.StatusCode)))
		}
		return "", fmt.Errorf("%d: %s", res.StatusCode, detail)
	}
	return text, nil
}

var injectedDashboardToken = regexp.MustCompile(`window\.__(?:ICEBATH|HERMES)_SESSION_TOKEN__\s*=\s*("(?:\\.|[^"\\])*")`)

// Session token the dashboard page injects, empty when absent or not a JSON string literal
func ExtractInjectedDashboardToken(html string) string {
	match := injectedDashboardToken.FindStringSubmatch(html)
	if match == nil {
		return ""
	}
	var token string
	if err := json.Unmarshal([]byte(match[1]), &token); err != nil {
		return ""
	}
	return token
}

// Dashboard index URL of a base URL, trailing slashes collapsed to one
func DashboardIndexURL(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + "/"
}

// Token the dashboard serves, the fallback when it serves none, logging a drift from the fallback
func ResolveServedDashboardToken(baseURL, fallbackToken string, timeout time.Duration, rememberLog func(string)) (string, error) {
	if timeout == 0 {
		timeout = DefaultTokenFetchTimeout
	}
	html, err := fetchPublicText(DashboardIndexURL(baseURL), timeout)
	if err != nil {
		return "", err
	}
	served := ExtractInjectedDashboardToken(html)
	if served != "" && served != fallbackToken && rememberLog != nil {
		rememberLog("[boot] dashboard served a different session token; using served token for WebSocket auth")
	}
	if served != "" {
		return served, nil
	}
	return fallbackToken, nil
}

// A served token differing from the spawn token while the child is dead belongs to a process the desktop did not spawn
func IsForeignBackendToken(servedToken, spawnToken string, childAlive bool) bool {
	return servedToken != "" && servedToken != spawnToken && !childAlive
}

// AdoptTokenOptions sample child liveness after the fetch and name the backend in logs and errors
type AdoptTokenOptions struct {
	ChildAlive  func() bool
	Label       string
	Timeout     time.Duration
	RememberLog func(string)
}

// Adopts the token the backend serves, keeping the spawn token when unreadable and refusing a foreign backend
func AdoptServedDashboardToken(baseURL, spawnToken string, opts AdoptTokenOptions) (string, error) {
	label := opts.Label
	if label == "" {
		label = "Icebath backend"
	}
	served, err := ResolveServedDashboardToken(baseURL, spawnToken, opts.Timeout, opts.RememberLog)
	if err != nil {
		if opts.RememberLog != nil {
			opts.RememberLog(fmt.Sprintf("[boot] could not read served dashboard token (%s): %s", label, err.Error()))
		}
		served = spawnToken
	}
	if IsForeignBackendToken(served, spawnToken, opts.ChildAlive()) {
		return "", fmt.Errorf("%s exited and %s is served by a process we did not spawn; refusing its session token.", label, DashboardIndexURL(baseURL))
	}
	return served, nil
}
