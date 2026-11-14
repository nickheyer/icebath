package gitcred

import (
	"context"
	"encoding/base64"
	"errors"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

func patch[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// Authorization extraheaders bound to origin in a git env block in declared order
func authHeadersFor(env map[string]string, origin string) []string {
	count, _ := strconv.Atoi(env["GIT_CONFIG_COUNT"])
	out := []string{}
	for i := 0; i < count; i++ {
		if env["GIT_CONFIG_KEY_"+strconv.Itoa(i)] == "http."+origin+"/.extraheader" {
			out = append(out, env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)])
		}
	}
	return out
}

func basic(user, pass string) string {
	return "Authorization: basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func refusedRun() runResult {
	return runResult{code: 128, stderr: "fatal: could not read Username for 'https://github.com': terminal prompts disabled\n"}
}

func fixedCreds(creds ...Credential) func(context.Context, string) iter.Seq2[Credential, error] {
	return func(context.Context, string) iter.Seq2[Credential, error] {
		return func(yield func(Credential, error) bool) {
			for _, c := range creds {
				if !yield(c, nil) {
					return
				}
			}
		}
	}
}

func gitRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// Local bare repo with one commit so a redirected clone succeeds
func seedBareUpstream(t *testing.T, dir string) string {
	upstream := filepath.Join(dir, "upstream.git")
	gitRun(t, "init", "-q", "--bare", upstream)
	work := filepath.Join(dir, "work")
	gitRun(t, "clone", "-q", upstream, work)
	if err := os.WriteFile(filepath.Join(work, "plugin.yaml"), []byte("name: probe\ndescription: d\nversion: '1'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, "-C", work, "-c", "user.name=t", "-c", "user.email=t@t", "add", ".")
	gitRun(t, "-C", work, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "i")
	gitRun(t, "-C", work, "push", "-q", "origin", "HEAD")
	return upstream
}

func TestPrivateCloneFallsBackToAuthAfterCredentialRequiredError(t *testing.T) {
	dir := t.TempDir()
	upstream := seedBareUpstream(t, dir)
	target := "https://git.example.test/acme/probe.git"
	var cloneCalls [][]string
	patch(t, &runProcess, func(ctx context.Context, spec runSpec) (runResult, error) {
		if slices.Contains(spec.argv, "clone") {
			cloneCalls = append(cloneCalls, authHeadersFor(spec.env, "https://git.example.test"))
			if len(cloneCalls) == 1 {
				return runResult{code: 128, stderr: "fatal: could not read Username for 'https://git.example.test': terminal prompts disabled\n"}, nil
			}
			argv := slices.Clone(spec.argv)
			for i, a := range argv {
				if a == target {
					argv[i] = upstream
				}
			}
			spec.argv = argv
		}
		return runText(ctx, spec)
	})
	patch(t, &resolveAuth, func(context.Context, string) (BasicAuth, bool, error) {
		return BasicAuth{"alice", "s3cret"}, true, nil
	})
	dest := filepath.Join(dir, "clone")
	code, stderr, err := runFallback(context.Background(), []string{"git", "clone", "--depth", "1", target, dest}, target, tools.NoninteractiveGitEnv(nil))
	if err != nil || code != 0 {
		t.Fatalf("clone: %d %v %s", code, err, stderr)
	}
	if len(cloneCalls) != 2 || len(cloneCalls[0]) != 0 || !slices.Equal(cloneCalls[1], []string{basic("alice", "s3cret")}) {
		t.Fatalf("clone calls %v", cloneCalls)
	}
	cfg, err := os.ReadFile(filepath.Join(dest, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), "s3cret") || strings.Contains(string(cfg), base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))) {
		t.Fatalf("credential persisted into .git/config:\n%s", cfg)
	}
	base := tools.NoninteractiveGitEnv(nil)
	got, err := WithGitAuth(context.Background(), base, "git@github.com:acme/probe.git", nil)
	if err != nil || !mapsEqual(got, base) {
		t.Fatalf("non https url changed the env: %v", err)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func TestPublicCloneAttemptsAnonymouslyWhenCredentialResolves(t *testing.T) {
	dir := t.TempDir()
	upstream := seedBareUpstream(t, dir)
	target := "https://github.com/robbyczgw-cla/hermes-web-search-plus.git"
	var cloneCalls [][]string
	patch(t, &runProcess, func(ctx context.Context, spec runSpec) (runResult, error) {
		if slices.Contains(spec.argv, "clone") {
			cloneCalls = append(cloneCalls, authHeadersFor(spec.env, "https://github.com"))
			argv := slices.Clone(spec.argv)
			for i, a := range argv {
				if a == target {
					argv[i] = upstream
				}
			}
			spec.argv = argv
		}
		return runText(ctx, spec)
	})
	patch(t, &resolveAuth, func(_ context.Context, url string) (BasicAuth, bool, error) {
		if strings.Contains(url, "github.com") {
			return BasicAuth{"x-access-token", "ghp_fake"}, true, nil
		}
		return BasicAuth{}, false, nil
	})
	code, stderr, err := runFallback(context.Background(), []string{"git", "clone", target, filepath.Join(dir, "clone")}, target, tools.NoninteractiveGitEnv(nil))
	if err != nil || code != 0 {
		t.Fatalf("clone: %d %v %s", code, err, stderr)
	}
	if len(cloneCalls) != 1 || len(cloneCalls[0]) != 0 {
		t.Fatalf("public repo must clone in a single anonymous attempt, got %v", cloneCalls)
	}
}

func TestRefFetchAndUpdatePullAttachCredentialOnlyAfterAnonymousRefusal(t *testing.T) {
	publicURL := "https://github.com/acme/public-plugin.git"
	for _, verb := range []string{"fetch", "pull"} {
		for _, outcome := range []string{"ok", "refused", "not_found"} {
			t.Run(verb+"_"+outcome, func(t *testing.T) {
				var attempts [][]string
				patch(t, &runProcess, func(_ context.Context, spec runSpec) (runResult, error) {
					attempts = append(attempts, authHeadersFor(spec.env, "https://github.com"))
					if outcome == "refused" && len(attempts) == 1 {
						return refusedRun(), nil
					}
					if outcome == "not_found" {
						return runResult{code: 128, stderr: "fatal: repository '" + publicURL + "/' not found\n"}, nil
					}
					return runResult{stdout: "Already up to date.\n"}, nil
				})
				patch(t, &resolveAuth, func(context.Context, string) (BasicAuth, bool, error) {
					if outcome == "not_found" {
						t.Fatal("credential must not be resolved for a non-credential failure")
					}
					return BasicAuth{"x-access-token", "ghp_fake"}, true, nil
				})
				patch(t, &iterAuth, fixedCreds(Credential{"gh auth token", BasicAuth{"x-access-token", "ghp_fake"}}))
				code, stderr, err := runFallback(context.Background(), []string{"git", verb, "origin"}, publicURL, map[string]string{})
				if err != nil {
					t.Fatal(err)
				}
				if (code == 0) != (outcome != "not_found") || strings.Contains(stderr, "not found") != (outcome == "not_found") {
					t.Fatalf("code %d stderr %q", code, stderr)
				}
				want := [][]string{{}}
				if outcome == "refused" {
					want = [][]string{{}, {basic("x-access-token", "ghp_fake")}}
				}
				if !slices.EqualFunc(attempts, want, slices.Equal[[]string]) {
					t.Fatalf("attempts %v want %v", attempts, want)
				}
			})
		}
	}
}

func TestAnonymousAttemptFailsFastUnderInheritedAskpass(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell askpass script")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="probe"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	dir := t.TempDir()
	url := srv.URL + "/acme/private.git"
	askpass := filepath.Join(dir, "askpass.sh")
	if err := os.WriteFile(askpass, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_ASKPASS", askpass)
	patch(t, &withAuth, func(_ context.Context, env map[string]string, _ string, _ *BasicAuth) (map[string]string, error) {
		out := map[string]string{"ICEBATH_TEST_AUTH_ATTACHED": "1"}
		for k, v := range env {
			out[k] = v
		}
		return out, nil
	})
	var attempts []map[string]string
	patch(t, &runProcess, func(ctx context.Context, spec runSpec) (runResult, error) {
		attempts = append(attempts, spec.env)
		return runText(ctx, spec)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	code, stderr, err := runFallback(ctx, []string{"git", "clone", url, filepath.Join(dir, "dest")}, url, tools.NoninteractiveGitEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if code == 0 || !strings.Contains(stderr, "could not read Username") {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if len(attempts) != 2 {
		t.Fatalf("anonymous refusal must be classified and the credential fallback must fire, got %d attempts", len(attempts))
	}
	for _, env := range attempts {
		if _, ok := env["GIT_ASKPASS"]; ok {
			t.Fatal("GIT_ASKPASS reached a git attempt")
		}
	}
}

func TestCredentialFillUsesStoredHelperAndNeverPrompts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell credential helper script")
	}
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper.sh")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n[ \"$1\" = get ] && printf 'username=bob\\npassword=pw-from-helper\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitconfig := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(gitconfig, []byte("[credential \"https://git.example.test\"]\n\thelper = !"+helper+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
	t.Setenv("GIT_ASKPASS", "/nonexistent/askpass-must-not-run")
	auth, ok, err := ResolveGitBasicAuth(context.Background(), "https://git.example.test/acme/x.git")
	if err != nil || !ok || auth != (BasicAuth{"bob", "pw-from-helper"}) {
		t.Fatalf("got %v %v %v", auth, ok, err)
	}
	start := time.Now()
	if _, ok, err := ResolveGitBasicAuth(context.Background(), "https://nothing.example.test/x.git"); ok || err != nil {
		t.Fatalf("unknown host resolved %v %v", ok, err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("credential fill for an unknown host did not fail fast")
	}
}

func TestRejectedEnvTokenFallsBackToTheNextOwnedCredential(t *testing.T) {
	patch(t, &iterAuth, fixedCreds(
		Credential{EnvTokenSource, BasicAuth{"x-access-token", "ghp_dead"}},
		Credential{"gh auth token", BasicAuth{"x-access-token", "gho_live"}},
		Credential{"git credential helper", BasicAuth{"bob", "never-needed"}}))
	patch(t, &resolveAuth, ResolveGitBasicAuth)
	var attempts [][]string
	patch(t, &runProcess, func(_ context.Context, spec runSpec) (runResult, error) {
		headers := authHeadersFor(spec.env, "https://github.com")
		attempts = append(attempts, headers)
		if slices.Equal(headers, []string{basic("x-access-token", "gho_live")}) {
			return runResult{}, nil
		}
		return refusedRun(), nil
	})
	url := "https://github.com/acme/private.git"
	code, _, err := runFallback(context.Background(), []string{"git", "clone", url, "dest"}, url, tools.NoninteractiveGitEnv(nil))
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	want := [][]string{{}, {basic("x-access-token", "ghp_dead")}, {basic("x-access-token", "gho_live")}}
	if !slices.EqualFunc(attempts, want, slices.Equal[[]string]) {
		t.Fatalf("attempts %v", attempts)
	}
}

func TestEveryCredentialRejectedNamesTheDeadEnvToken(t *testing.T) {
	patch(t, &iterAuth, fixedCreds(
		Credential{EnvTokenSource, BasicAuth{"x-access-token", "ghp_dead"}},
		Credential{"gh auth token", BasicAuth{"x-access-token", "gho_dead_too"}}))
	patch(t, &runProcess, func(context.Context, runSpec) (runResult, error) { return refusedRun(), nil })
	url := "https://github.com/acme/private.git"
	code, stderr, err := runFallback(context.Background(), []string{"git", "clone", url, "dest"}, url, tools.NoninteractiveGitEnv(nil))
	if err != nil || code == 0 || !strings.Contains(stderr, "GITHUB_TOKEN/GH_TOKEN in your .env was rejected") {
		t.Fatalf("code %d err %v stderr %q", code, err, stderr)
	}
	var calls []map[string]string
	patch(t, &runProcess, func(_ context.Context, spec runSpec) (runResult, error) {
		calls = append(calls, spec.env)
		if len(authHeadersFor(spec.env, "https://github.com")) > 0 {
			return runResult{code: 128, stderr: "fatal: repository '" + url + "/' not found\n"}, nil
		}
		return refusedRun(), nil
	})
	_, stderr, err = runFallback(context.Background(), []string{"git", "clone", url, "dest"}, url, tools.NoninteractiveGitEnv(nil))
	if err != nil || len(calls) != 2 || !strings.Contains(stderr, "not found") || strings.Contains(stderr, "hint:") {
		t.Fatalf("calls %d err %v stderr %q", len(calls), err, stderr)
	}
}

func TestIterGitBasicAuthOrderDedupAndScopedSecretErrors(t *testing.T) {
	secrets := map[string]string{"GH_TOKEN": "same"}
	SetSecretReader(func(_ context.Context, name string) (string, error) { return secrets[name], nil })
	t.Cleanup(func() { SetSecretReader(nil) })
	var ghCalls, fillCalls int
	var origins []string
	patch(t, &ghTokenLookup, func() string { ghCalls++; return "same" })
	patch(t, &credentialFill, func(_ context.Context, origin string) (BasicAuth, bool) {
		fillCalls++
		origins = append(origins, origin)
		return BasicAuth{"bob", "pw"}, true
	})
	var got []Credential
	for c, err := range IterGitBasicAuth(context.Background(), "https://GitHub.com/acme/x.git") {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, c)
	}
	want := []Credential{{EnvTokenSource, BasicAuth{"x-access-token", "same"}}, {"git credential helper", BasicAuth{"bob", "pw"}}}
	if !slices.Equal(got, want) || ghCalls != 1 || fillCalls != 1 {
		t.Fatalf("got %v gh %d fill %d", got, ghCalls, fillCalls)
	}
	for c := range IterGitBasicAuth(context.Background(), "https://github.com/x") {
		if c.Source != EnvTokenSource {
			t.Fatalf("first source %q", c.Source)
		}
		break
	}
	if ghCalls != 1 || fillCalls != 1 {
		t.Fatal("a stopped iteration still ran later lookups")
	}
	for c := range IterGitBasicAuth(context.Background(), "https://gitlab.com:8443/x") {
		if c.Source != "git credential helper" {
			t.Fatalf("non GitHub host offered %q", c.Source)
		}
	}
	if !slices.Equal(origins, []string{"https://github.com", "https://gitlab.com:8443"}) {
		t.Fatalf("fill origins %v", origins)
	}
	if n := len(slices.Collect(func(yield func(Credential) bool) {
		for c := range IterGitBasicAuth(context.Background(), "http://github.com/x") {
			yield(c)
		}
	})); n != 0 {
		t.Fatalf("plain http offered %d credentials", n)
	}
	unscoped := errors.New("UnscopedSecretError")
	SetSecretReader(func(context.Context, string) (string, error) { return "", unscoped })
	patch(t, &runProcess, func(context.Context, runSpec) (runResult, error) { return refusedRun(), nil })
	if _, _, err := runFallback(context.Background(), []string{"git", "clone"}, "https://github.com/a/b", nil); !errors.Is(err, unscoped) {
		t.Fatalf("unscoped secret error swallowed: %v", err)
	}
	SetSecretReader(nil)
	if _, _, err := ResolveGitBasicAuth(context.Background(), "https://github.com/a/b"); !errors.Is(err, ErrSecretReaderUnregistered) {
		t.Fatalf("unregistered reader: %v", err)
	}
}

func TestWithGitAuthAppendsExtraheaderAndOriginShapes(t *testing.T) {
	env := map[string]string{"GIT_CONFIG_COUNT": "2", "KEEP": "1"}
	got, err := WithGitAuth(context.Background(), env, "https://user@[::1]:8443/x.git", &BasicAuth{"u", "p"})
	if err != nil {
		t.Fatal(err)
	}
	if got["GIT_CONFIG_KEY_2"] != "http.https://[::1]:8443/.extraheader" || got["GIT_CONFIG_VALUE_2"] != basic("u", "p") ||
		got["GIT_CONFIG_COUNT"] != "3" || got["KEEP"] != "1" || env["GIT_CONFIG_COUNT"] != "2" {
		t.Fatalf("got %v", got)
	}
	if _, err := WithGitAuth(context.Background(), map[string]string{"GIT_CONFIG_COUNT": "x"}, "https://h/x", &BasicAuth{"u", "p"}); err == nil ||
		err.Error() != "invalid literal for int() with base 10: 'x'" {
		t.Fatalf("bad count: %v", err)
	}
	if _, err := WithGitAuth(context.Background(), nil, "https://h:99999/x", &BasicAuth{"u", "p"}); err == nil {
		t.Fatal("out of range port must raise")
	}
	for text, want := range map[string]bool{
		"fatal: Authentication failed for 'https://x/'":  true,
		"error: The requested URL returned error: 403":   true,
		"remote: HTTP 401 ":                              true,
		"error: The requested URL returned error: 404":   false,
		"fatal: repository 'https://x/' not found":       false,
		"error: The requested URL returned error: 4010":  false,
		"fatal: COULD NOT READ PASSWORD for 'https://x'": true,
		"status 403\xc3\xa9":                             false,
	} {
		if IsCredentialRequiredError(text, "") != want {
			t.Errorf("%q: want %v", text, want)
		}
	}
}

// Exit code and stderr of a fallback run through the default runner
func runFallback(ctx context.Context, argv []string, url string, env map[string]string) (int, string, error) {
	r, err := RunGitWithCredentialFallback(ctx, argv, url, env, nil)
	return r.Code, r.Stderr, err
}
