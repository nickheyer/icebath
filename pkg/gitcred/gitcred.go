package gitcred

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"maps"
	"math/big"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/tools"
)

var githubHosts = map[string]bool{"github.com": true, "gist.github.com": true}

// EnvTokenSource names the GITHUB_TOKEN or GH_TOKEN candidate in rejection lists and the stale token hint
const EnvTokenSource = "GITHUB_TOKEN/GH_TOKEN"

// BasicAuth is the username and password pair git sends as an Authorization basic header
type BasicAuth struct{ Username, Password string }

// Credential is one owned credential with the source that supplied it
type Credential struct {
	Source string
	Auth   BasicAuth
}

// ErrSecretReaderUnregistered is returned when a GitHub env token lookup runs before pkg/provider registered its reader
var ErrSecretReaderUnregistered = errors.New("gitcred: no secret reader registered, import pkg/provider")

var (
	secretMu     sync.RWMutex
	secretReader func(ctx context.Context, name string) (string, error)
)

// Installs the profile scoped secret lookup that GITHUB_TOKEN and GH_TOKEN resolve through
func SetSecretReader(fn func(ctx context.Context, name string) (string, error)) {
	secretMu.Lock()
	secretReader = fn
	secretMu.Unlock()
}

func readSecret(ctx context.Context, name string) (string, error) {
	secretMu.RLock()
	fn := secretReader
	secretMu.RUnlock()
	if fn == nil {
		return "", ErrSecretReaderUnregistered
	}
	return fn(ctx, name)
}

type runResult struct {
	code           int
	stdout, stderr string
}

type runSpec struct {
	argv    []string
	env     map[string]string
	input   *string
	timeout time.Duration
	hide    bool
}

// Seams over subprocess.run and the module functions the hermes tests patch
var (
	runProcess     = runText
	iterAuth       = IterGitBasicAuth
	withAuth       = WithGitAuth
	resolveAuth    = ResolveGitBasicAuth
	lookPath       = exec.LookPath
	ghTokenLookup  = GhCliToken
	credentialFill = fillCredential
)

func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func translateNewlines(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// Runs argv in text mode like subprocess.run with captured output, utf-8 replace decoding and universal newlines
func runText(ctx context.Context, spec runSpec) (runResult, error) {
	if spec.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, spec.argv[0], spec.argv[1:]...)
	cmd.Env = envList(spec.env)
	cmd.WaitDelay = 5 * time.Second
	if spec.input != nil {
		in := *spec.input
		if runtime.GOOS == "windows" {
			in = strings.ReplaceAll(in, "\n", "\r\n")
		}
		cmd.Stdin = strings.NewReader(in)
	}
	if spec.hide {
		hideWindow(cmd)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return runResult{}, fmt.Errorf("Command '%s' timed out: %w", pyrepr.Repr(anyList(spec.argv)), ctx.Err())
	}
	res := runResult{
		stdout: translateNewlines(pystr.DecodeUTF8Replace(stdout.Bytes())),
		stderr: translateNewlines(pystr.DecodeUTF8Replace(stderr.Bytes())),
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.code = exitErr.ExitCode()
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.code = -int(ws.Signal())
		}
		return res, nil
	}
	if err != nil {
		return runResult{}, err
	}
	return res, nil
}

func anyList(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// Origin of an https URL with brackets around IPv6 hosts and any explicit port, absent for other schemes
func httpsOrigin(raw string) (string, bool, error) {
	u, err := pyurl.Split(raw)
	if err != nil {
		return "", false, err
	}
	host := u.Hostname()
	if u.Scheme != "https" || host == "" {
		return "", false, nil
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port, hasPort, err := u.Port()
	if err != nil {
		return "", false, err
	}
	if hasPort {
		host += ":" + strconv.Itoa(port)
	}
	return "https://" + host, true, nil
}

// GITHUB_TOKEN then GH_TOKEN from the active profile secret scope, empty when neither is set
func envGitHubToken(ctx context.Context) (string, error) {
	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		v, err := readSecret(ctx, name)
		if err != nil {
			return "", err
		}
		if v != "" {
			return v, nil
		}
	}
	return "", nil
}

// Token from the gh CLI keyring login with exported GitHub tokens hidden, empty when gh is absent or fails
func GhCliToken() string {
	gh, err := lookPath("gh")
	if err != nil || gh == "" {
		return ""
	}
	env := tools.NoninteractiveGitEnv(nil)
	env["GH_PROMPT_DISABLED"] = "1"
	delete(env, "GH_TOKEN")
	delete(env, "GITHUB_TOKEN")
	res, err := runProcess(context.Background(), runSpec{argv: []string{gh, "auth", "token"}, env: env, timeout: 10 * time.Second, hide: true})
	if err != nil {
		slog.Debug(fmt.Sprintf("gh auth token lookup failed: %v", err))
		return ""
	}
	if res.code != 0 {
		return ""
	}
	return pystr.Strip(res.stdout)
}

// Username and password the user's git credential helpers store for origin, never prompting
func fillCredential(ctx context.Context, origin string) (BasicAuth, bool) {
	git, err := lookPath("git")
	if err != nil || git == "" {
		return BasicAuth{}, false
	}
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	env["GIT_TERMINAL_PROMPT"] = "0"
	env["GCM_INTERACTIVE"] = "Never"
	delete(env, "GIT_ASKPASS")
	delete(env, "SSH_ASKPASS")
	parsed, err := pyurl.Split(origin)
	if err != nil {
		slog.Debug(fmt.Sprintf("git credential fill failed for %s: %v", origin, err))
		return BasicAuth{}, false
	}
	request := "protocol=https\nhost=" + parsed.Netloc + "\n\n"
	res, err := runProcess(ctx, runSpec{argv: []string{git, "-c", "core.askPass=", "credential", "fill"}, env: env,
		input: &request, timeout: 15 * time.Second, hide: true})
	if err != nil {
		slog.Debug(fmt.Sprintf("git credential fill failed for %s: %v", origin, err))
		return BasicAuth{}, false
	}
	if res.code != 0 {
		return BasicAuth{}, false
	}
	fields := map[string]string{}
	for _, line := range pystr.Splitlines(res.stdout, false) {
		if k, v, ok := strings.Cut(line, "="); ok {
			fields[k] = v
		}
	}
	if fields["password"] != "" {
		return BasicAuth{Username: fields["username"], Password: fields["password"]}, true
	}
	return BasicAuth{}, false
}

// Every credential the user owns for the URL host in precedence order without duplicates, looked up lazily
func IterGitBasicAuth(ctx context.Context, url string) iter.Seq2[Credential, error] {
	return func(yield func(Credential, error) bool) {
		origin, ok, err := httpsOrigin(url)
		if err != nil {
			yield(Credential{}, err)
			return
		}
		if !ok {
			return
		}
		type source struct {
			name   string
			lookup func() (BasicAuth, bool, error)
		}
		tokenPair := func(token string) (BasicAuth, bool, error) {
			if token == "" {
				return BasicAuth{}, false, nil
			}
			return BasicAuth{Username: "x-access-token", Password: token}, true, nil
		}
		var sources []source
		parsed, err := pyurl.Split(origin)
		if err != nil {
			yield(Credential{}, err)
			return
		}
		if githubHosts[parsed.Hostname()] {
			sources = append(sources,
				source{EnvTokenSource, func() (BasicAuth, bool, error) {
					token, err := envGitHubToken(ctx)
					if err != nil {
						return BasicAuth{}, false, err
					}
					return tokenPair(token)
				}},
				source{"gh auth token", func() (BasicAuth, bool, error) { return tokenPair(ghTokenLookup()) }})
		}
		sources = append(sources, source{"git credential helper", func() (BasicAuth, bool, error) {
			auth, ok := credentialFill(ctx, origin)
			return auth, ok, nil
		}})
		seen := map[BasicAuth]bool{}
		for _, src := range sources {
			auth, ok, err := src.lookup()
			if err != nil {
				yield(Credential{}, err)
				return
			}
			if ok && !seen[auth] {
				seen[auth] = true
				if !yield(Credential{Source: src.name, Auth: auth}, nil) {
					return
				}
			}
		}
	}
}

// First credential the user owns for the URL, absent for non https URLs or when none is stored
func ResolveGitBasicAuth(ctx context.Context, url string) (BasicAuth, bool, error) {
	for cred, err := range iterAuth(ctx, url) {
		if err != nil {
			return BasicAuth{}, false, err
		}
		return cred.Auth, true, nil
	}
	return BasicAuth{}, false, nil
}

// Copy of env carrying an http extraheader for the URL origin when a credential is given or stored
func WithGitAuth(ctx context.Context, env map[string]string, url string, auth *BasicAuth) (map[string]string, error) {
	out := maps.Clone(env)
	if out == nil {
		out = map[string]string{}
	}
	origin, ok, err := httpsOrigin(url)
	if err != nil {
		return nil, err
	}
	if !ok {
		return out, nil
	}
	if auth == nil {
		resolved, found, err := resolveAuth(ctx, url)
		if err != nil {
			return nil, err
		}
		if !found {
			return out, nil
		}
		auth = &resolved
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(auth.Username + ":" + auth.Password))
	countText := out["GIT_CONFIG_COUNT"]
	if countText == "" {
		countText = "0"
	}
	idx, valid := pyint.Parse(countText)
	if !valid {
		return nil, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.StrRepr(countText))
	}
	n := idx.String()
	out["GIT_CONFIG_KEY_"+n] = "http." + origin + "/.extraheader"
	out["GIT_CONFIG_VALUE_"+n] = "Authorization: basic " + encoded
	out["GIT_CONFIG_COUNT"] = new(big.Int).Add(idx, big.NewInt(1)).String()
	return out, nil
}

func authHeaders(env map[string]string) map[string]bool {
	out := map[string]bool{}
	for k, v := range env {
		if strings.HasPrefix(k, "GIT_CONFIG_VALUE_") && strings.HasPrefix(v, "Authorization:") {
			out[v] = true
		}
	}
	return out
}

var credentialRequiredRE = sync.OnceValue(func() *regexp2.Regexp {
	re, err := pyre.Compile(`(?i)could not read (?:username|password)|terminal prompts disabled|authentication failed` +
		`|(?:error|status|http)[: ]+40[13]\b`)
	if err != nil {
		panic(err)
	}
	return re
})

// Reports whether failed git output says the remote demands a credential rather than another failure
func IsCredentialRequiredError(stderr, stdout string) bool {
	ok, err := credentialRequiredRE().MatchString(stderr + "\n" + stdout)
	return err == nil && ok
}

// Runs a git network verb anonymously, then with each owned credential while the remote keeps refusing for credentials
func RunGitWithCredentialFallback(ctx context.Context, argv []string, url string, env map[string]string) (exitCode int, stderr string, err error) {
	env = maps.Clone(env)
	if env == nil {
		env = map[string]string{}
	}
	delete(env, "GIT_ASKPASS")
	delete(env, "SSH_ASKPASS")
	run := func(e map[string]string) (runResult, error) {
		return runProcess(ctx, runSpec{argv: argv, env: e})
	}
	refused := func(r runResult) bool { return r.code != 0 && IsCredentialRequiredError(r.stderr, r.stdout) }
	result, err := run(env)
	if err != nil {
		return 0, "", err
	}
	if result.code == 0 || url == "" || !IsCredentialRequiredError(result.stderr, result.stdout) {
		return result.code, result.stderr, nil
	}
	authEnv, err := withAuth(ctx, env, url, nil)
	if err != nil {
		return 0, "", err
	}
	if maps.Equal(authEnv, env) {
		return result.code, result.stderr, nil
	}
	if result, err = run(authEnv); err != nil {
		return 0, "", err
	}
	sent := authHeaders(authEnv)
	var rejected []string
	for cred, iterErr := range iterAuth(ctx, url) {
		if iterErr != nil {
			return 0, "", iterErr
		}
		if !refused(result) {
			break
		}
		auth := cred.Auth
		candidateEnv, err := withAuth(ctx, env, url, &auth)
		if err != nil {
			return 0, "", err
		}
		headers := authHeaders(candidateEnv)
		subset := true
		for h := range headers {
			if !sent[h] {
				subset = false
				break
			}
		}
		if subset {
			rejected = append(rejected, cred.Source)
			continue
		}
		for h := range headers {
			sent[h] = true
		}
		origin, _, _ := httpsOrigin(url)
		from := strings.Join(rejected, ", ")
		if from == "" {
			from = "the stored credential"
		}
		slog.Warn(fmt.Sprintf("%s rejected the credential from %s; retrying with %s", origin, from, cred.Source))
		if result, err = run(candidateEnv); err != nil {
			return 0, "", err
		}
		if refused(result) {
			rejected = append(rejected, cred.Source)
		}
	}
	if result.code != 0 && slices.Contains(rejected, EnvTokenSource) {
		result.stderr += "\nhint: the GITHUB_TOKEN/GH_TOKEN in your .env was rejected by GitHub;" +
			" replace it or remove it (gh auth login is used when it is absent)."
	}
	return result.code, result.stderr, nil
}
