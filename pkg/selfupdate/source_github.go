package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"time"

	"golang.org/x/net/http/httpproxy"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

const githubJSONAccept = "application/vnd.github+json"

var (
	sourceGitHubAPI      = "https://api.github.com"
	sourceRawBase        = "https://raw.githubusercontent.com"
	githubTokenSource    = plugins.GithubToken
	sourceRequestTimeout = 10 * time.Second
)

// Read only git probe environment that never prompts, takes optional locks, lazy fetches or inherits targeting
func sourceGitEnv() []string {
	env := tools.NoninteractiveGitEnv(nil)
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_SHALLOW_FILE", "GIT_NAMESPACE",
		"GIT_LITERAL_PATHSPECS", "GIT_GLOB_PATHSPECS", "GIT_NOGLOB_PATHSPECS", "GIT_ICASE_PATHSPECS"} {
		delete(env, key)
	}
	env["GIT_OPTIONAL_LOCKS"] = "0"
	env["GIT_NO_LAZY_FETCH"] = "1"
	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	sort.Strings(out)
	return out
}

type sourceGitResult struct {
	ReturnCode int
	Stdout     string
	Stderr     string
}

// Git child that outlived its probe timeout
type sourceGitTimeout struct {
	Argv    []string
	Timeout time.Duration
}

func (e *sourceGitTimeout) Error() string {
	return fmt.Sprintf("Command %s timed out after %g seconds", pyListRepr(e.Argv), e.Timeout.Seconds())
}

func pyListRepr(argv []string) string {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, arg := range argv {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("'" + arg + "'")
	}
	b.WriteByte(']')
	return b.String()
}

// Git runner of every source probe, swapped by tests that record the commands a check runs
var sourceGitRun = execSourceGit

// Runs argv in cwd with the probe environment and null stdin, an error when it cannot start or times out
func execSourceGit(ctx context.Context, argv []string, cwd string, timeout time.Duration) (*sourceGitResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = sourceGitEnv()
	cmd.WaitDelay = time.Second
	hideProbeWindow(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if runCtx.Err() != nil && ctx.Err() == nil {
		return nil, &sourceGitTimeout{Argv: argv, Timeout: timeout}
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return nil, err
	}
	return &sourceGitResult{ReturnCode: cmd.ProcessState.ExitCode(),
		Stdout: pystr.DecodeUTF8Replace(stdout.Bytes()), Stderr: pystr.DecodeUTF8Replace(stderr.Bytes())}, nil
}

// GitHub HTTP failure tagged with whether the failing request carried a token
type githubRequestError struct {
	Err           error
	Authenticated bool
}

func (e *githubRequestError) Error() string { return e.Err.Error() }
func (e *githubRequestError) Unwrap() error { return e.Err }

// Whether the request that failed carried a token, false when no request recorded it
func failureAuthenticated(err error) bool {
	var tagged *githubRequestError
	return errors.As(err, &tagged) && tagged.Authenticated
}

// GETs an api.github.com resource with the credential ladder, retrying anonymously when GitHub rejects the token
func sourceRequest(ctx context.Context, target, accept string) (string, error) {
	token, authenticated := githubTokenSource()
	text, err := sourceRequestWith(ctx, target, accept, token)
	if err == nil {
		return text, nil
	}
	var httpErr *plugins.GithubHTTPError
	if !errors.As(err, &httpErr) {
		return "", err
	}
	spent := httpErr.Header != nil && httpErr.Header.Get("x-ratelimit-remaining") == "0"
	if !authenticated || (httpErr.Code != 401 && httpErr.Code != 403) || spent {
		return "", &githubRequestError{Err: err, Authenticated: authenticated}
	}
	slog.Debug("GitHub rejected the configured token; retrying anonymously")
	text, err = sourceRequestWith(ctx, target, accept, "")
	if err != nil && errors.As(err, &httpErr) {
		return "", &githubRequestError{Err: err, Authenticated: false}
	}
	return text, err
}

// One GET with an optional bearer token, the stripped UTF-8 body or the HTTP or transport failure
func sourceRequestWith(ctx context.Context, target, accept, token string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "icebath-update-check")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	proxy := httpproxy.FromEnvironment().ProxyFunc()
	client := &http.Client{Timeout: sourceRequestTimeout, Transport: &http.Transport{
		Proxy:       func(r *http.Request) (*url.URL, error) { return proxy(r.URL) },
		DialContext: (&net.Dialer{Timeout: sourceRequestTimeout}).DialContext,
	}}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if httpErr := plugins.GithubHTTPErrorFromResponse(resp); httpErr != nil {
		return "", httpErr
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return "", err
	}
	text, err := pystr.DecodeUTF8Sig(body)
	if err != nil {
		return "", err
	}
	return pystr.Strip(text), nil
}

// Decoded JSON of an api.github.com resource read through the credential ladder
func sourceJSON(ctx context.Context, target string) (any, error) {
	text, err := sourceRequest(ctx, target, githubJSONAccept)
	if err != nil {
		return nil, err
	}
	return pyjson.Loads(text)
}

// Reports whether value is a 40 digit hex commit id in either case
func isFullSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// GitHub comparison of two full commits, nil when either is not a full sha or GitHub did not answer
func githubCompare(ctx context.Context, currentRev, targetRev, repository string) *pyjson.Object {
	if !isFullSHA(currentRev) || !isFullSHA(targetRev) {
		return nil
	}
	payload, err := sourceJSON(ctx, fmt.Sprintf("%s/repos/%s/compare/%s...%s", sourceGitHubAPI, repository, currentRev, targetRev))
	if err != nil {
		return nil
	}
	obj, _ := payload.(*pyjson.Object)
	return obj
}

// Commits target is ahead of current by GitHub's comparison, -1 when unknown
func githubCompareBehind(ctx context.Context, currentRev, targetRev, repository string) int {
	payload := githubCompare(ctx, currentRev, targetRev, repository)
	if payload == nil {
		return -1
	}
	if ahead, ok := pyIntValue(stampGet(payload, "ahead_by")); ok && ahead >= 0 {
		return int(ahead)
	}
	return -1
}

// Integer value of a decoded JSON number Python reads as int, never a float or a bool
func pyIntValue(value any) (int64, bool) {
	switch n := value.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64, bool:
		return 0, false
	case fmt.Stringer:
		parsed, err := strconv.ParseInt(n.String(), 10, 64)
		return parsed, err == nil
	}
	return 0, false
}
