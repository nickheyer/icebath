package plugins

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Environment variables the update check reads a GitHub token from, in order
var GithubTokenEnvVars = []string{"GITHUB_TOKEN", "GH_TOKEN"}

const ghCLITimeout = 3 * time.Second

var (
	ghCLIMu     sync.Mutex
	ghCLICache  string
	ghCLIProbed bool
	ghCLIToken  = probeGhCLIToken
)

// First non blank environment token trimmed, a blank value falling through to the next
func GithubTokenFromEnv(getenv func(string) string) (string, bool) {
	if getenv == nil {
		getenv = os.Getenv
	}
	for _, name := range GithubTokenEnvVars {
		if value := pystr.Strip(getenv(name)); value != "" {
			return value, true
		}
	}
	return "", false
}

// Token the gh CLI login answers, probed once per process
func probeGhCLIToken() (string, bool) {
	ghCLIMu.Lock()
	defer ghCLIMu.Unlock()
	if ghCLIProbed {
		return ghCLICache, ghCLICache != ""
	}
	ghCLIProbed = true
	ctx, cancel := context.WithTimeout(context.Background(), ghCLITimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "auth", "token")
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		logDebug(fmt.Sprintf("gh CLI token lookup unavailable: %s", err))
		return "", false
	}
	if ctx.Err() != nil {
		logDebug(fmt.Sprintf("gh CLI token lookup unavailable: Command '['gh', 'auth', 'token']' timed out after %d seconds", int(ghCLITimeout/time.Second)))
		return "", false
	}
	token := ""
	if err == nil {
		token = pystr.Strip(pystr.DecodeUTF8Replace(out))
	}
	ghCLICache = token
	return token, token != ""
}

// Credential for an api.github.com request, false for anonymous
func GithubToken() (string, bool) {
	if token, ok := GithubTokenFromEnv(nil); ok {
		return token, true
	}
	return ghCLIToken()
}

// HTTP error status api.github.com answered with its response headers
type GithubHTTPError struct {
	Code   int
	Header http.Header
}

func (e *GithubHTTPError) Error() string {
	return fmt.Sprintf("HTTP Error %d: %s", e.Code, http.StatusText(e.Code))
}

// Error for a non success api.github.com response, nil for a 2xx or 3xx status
func GithubHTTPErrorFromResponse(resp *http.Response) *GithubHTTPError {
	if resp.StatusCode < 400 {
		return nil
	}
	return &GithubHTTPError{Code: resp.StatusCode, Header: resp.Header}
}

// One actionable line for a failed api.github.com call naming the reset time and the GITHUB_TOKEN fix
func DescribeGithubFailure(err error, authenticated bool, now *float64) string {
	var httpErr *GithubHTTPError
	if errors.As(err, &httpErr) {
		status := httpErr.Code
		remaining, ok := githubHeaderInt(httpErr.Header, "x-ratelimit-remaining")
		if (status == 403 || status == 429) && ok && remaining == 0 {
			when := "within an hour"
			if reset, ok := githubHeaderInt(httpErr.Header, "x-ratelimit-reset"); ok {
				current := float64(time.Now().UnixNano()) / 1e9
				if now != nil {
					current = *now
				}
				minutes := int(pyFloorDiv(float64(reset)-current+59, 60))
				if minutes >= 1 {
					when = fmt.Sprintf("in about %d minutes", minutes)
					if minutes == 1 {
						when = "in about a minute"
					}
				}
			}
			if authenticated {
				return fmt.Sprintf("GitHub API rate limit reached for your GITHUB_TOKEN (HTTP %d) — it resets %s.", status, when)
			}
			return fmt.Sprintf("GitHub API rate limit reached (HTTP %d): anonymous requests are limited to 60 per hour "+
				"per network address, shared with everyone behind the same connection. It resets %s; "+
				"setting GITHUB_TOKEN in the environment lifts the limit.", status, when)
		}
		if status >= 500 {
			return fmt.Sprintf("GitHub is having trouble (HTTP %d from api.github.com) — check githubstatus.com and try again later.", status)
		}
		return fmt.Sprintf("api.github.com answered HTTP %d.", status)
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		reason := urlErr.Err
		if isTimeoutError(reason) || strings.Contains(pystr.Lower(fmt.Sprint(reason)), "timed out") {
			return "api.github.com did not answer within 10 seconds."
		}
		return fmt.Sprintf("Connection to api.github.com failed (%v) — check your connection, firewall or proxy.", reason)
	}
	if isTimeoutError(err) {
		return "api.github.com did not answer within 10 seconds."
	}
	return fmt.Sprintf("api.github.com: %v", err)
}

// Reports a timeout the way TimeoutError subclasses classify socket and deadline failures
func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	return errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout())
}

// Integer value of a header as int(str(value).strip()) reads it, false where that raises
func githubHeaderInt(header http.Header, name string) (int64, bool) {
	n, ok := pystr.IntText(pystr.Strip(header.Get(name)))
	if !ok || !n.IsInt64() {
		return 0, false
	}
	return n.Int64(), true
}

// Float floor division with the CPython rounding of float_floor_div
func pyFloorDiv(a, b float64) float64 {
	mod := math.Mod(a, b)
	div := (a - mod) / b
	if mod != 0 && (b < 0) != (mod < 0) {
		div -= 1
	}
	if div == 0 {
		return math.Copysign(0, a/b)
	}
	floor := math.Floor(div)
	if div-floor > 0.5 {
		floor += 1
	}
	return floor
}
