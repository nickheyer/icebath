package selfupdate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
)

const srSHA = "0123456789abcdef0123456789abcdef01234567"

func srGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Routes GitHub requests to a local server and reads tokens from the environment only
func srRoute(t *testing.T, handler http.Handler) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	api, raw, tokens, public := sourceGitHubAPI, sourceRawBase, githubTokenSource, sourcePublicBase
	sourceGitHubAPI, sourceRawBase, sourcePublicBase = server.URL, server.URL+"/raw", server.URL
	githubTokenSource = func() (string, bool) { return plugins.GithubTokenFromEnv(nil) }
	t.Cleanup(func() { sourceGitHubAPI, sourceRawBase, githubTokenSource, sourcePublicBase = api, raw, tokens, public })
	for _, name := range plugins.GithubTokenEnvVars {
		t.Setenv(name, "")
	}
}

type srAuthServer struct {
	mu                    sync.Mutex
	seen                  [][2]string
	limited, rejectTokens bool
}

func (s *srAuthServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	s.mu.Lock()
	s.seen = append(s.seen, [2]string{r.URL.Path, auth})
	limited, reject := s.limited, s.rejectTokens
	s.mu.Unlock()
	switch {
	case reject && auth != "":
		w.WriteHeader(401)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	case limited:
		w.Header().Set("x-ratelimit-remaining", "0")
		w.Header().Set("x-ratelimit-reset", "9999999999")
		w.WriteHeader(403)
		w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	case strings.HasSuffix(r.URL.Path, "/releases/latest"):
		w.Write([]byte(`{"tag_name": "v0.21.6", "draft": false, "prerelease": false}`))
	case strings.HasSuffix(r.URL.Path, "/commits/v0.21.6"):
		w.Write([]byte(`{"sha": "` + srSHA + `"}`))
	default:
		w.WriteHeader(404)
		w.Write([]byte(`{}`))
	}
}

func TestSourceStableLookupSendsTheConfiguredToken(t *testing.T) {
	server := &srAuthServer{}
	srRoute(t, server)
	t.Setenv("GH_TOKEN", "ghp_user")
	target, err := resolveStable(context.Background(), OfficialRepository, nil, "")
	if err != nil || target.Commit != srSHA || target.Version != "0.21.6" {
		t.Fatalf("target %+v %v", target, err)
	}
	if len(server.seen) == 0 {
		t.Fatal("no request reached GitHub")
	}
	for _, seen := range server.seen {
		if seen[1] != "Bearer ghp_user" {
			t.Fatalf("request %v carried %q", seen[0], seen[1])
		}
	}
}

func TestSourceStableRateLimitIsReportedAsOneNotAsAMissingRelease(t *testing.T) {
	for _, token := range []string{"", "ghp_user"} {
		server := &srAuthServer{limited: true}
		srRoute(t, server)
		t.Setenv("GH_TOKEN", token)
		_, err := resolveStable(context.Background(), OfficialRepository, nil, "")
		if !IsValueError(err) {
			t.Fatalf("token %q: want a ValueError, got %v", token, err)
		}
		message := err.Error()
		if !strings.Contains(message, "rate limit") || strings.Contains(message, "No published stable release") {
			t.Fatalf("token %q: %s", token, message)
		}
		if strings.Contains(message, "GITHUB_TOKEN in the environment") != (token == "") {
			t.Fatalf("token %q: %s", token, message)
		}
	}
}

func TestSourceStableRejectedTokenThenAnonymousLimitNamesTheAnonymousQuota(t *testing.T) {
	server := &srAuthServer{limited: true, rejectTokens: true}
	srRoute(t, server)
	t.Setenv("GH_TOKEN", "ghp_revoked")
	_, err := resolveStable(context.Background(), OfficialRepository, nil, "")
	if err == nil {
		t.Fatal("a spent anonymous quota must refuse")
	}
	if len(server.seen) != 2 || server.seen[0][1] != "Bearer ghp_revoked" || server.seen[1][1] != "" {
		t.Fatalf("requests %v", server.seen)
	}
	if !strings.Contains(err.Error(), "anonymous requests are limited") || strings.Contains(err.Error(), "for your GITHUB_TOKEN") {
		t.Fatalf("message %s", err)
	}
}

type srReleases struct {
	root, origin string
	commits      []string
	tags         map[string]string
	mu           sync.Mutex
	responses    map[string]any
	requests     []string
}

func (f *srReleases) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *srReleases) set(path string, value any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses[path] = value
}

func (f *srReleases) drop(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.responses, path)
}

// Origin with old, stable, canary and unpublished commits, a detached clone and a local GitHub
func srFixture(t *testing.T, bom bool) *srReleases {
	tmp := t.TempDir()
	f := &srReleases{origin: filepath.Join(tmp, "origin"), root: filepath.Join(tmp, "checkout"), responses: map[string]any{},
		tags: map[string]string{"stable": "v1.2.3", "canary": "v1.2.3+canary.20260911T125822Z"}}
	os.MkdirAll(f.origin, 0o755)
	srGit(t, f.origin, "init", "-b", "main")
	srGit(t, f.origin, "config", "user.name", "Release Fixture")
	srGit(t, f.origin, "config", "user.email", "fixture@example.invalid")
	srGit(t, f.origin, "config", "commit.gpgsign", "false")
	srGit(t, f.origin, "config", "tag.gpgsign", "false")
	for _, label := range []string{"old", "stable", "canary", "unpublished"} {
		os.WriteFile(filepath.Join(f.origin, "content.txt"), []byte(label), 0o644)
		srGit(t, f.origin, "add", ".")
		srGit(t, f.origin, "commit", "-m", label)
		f.commits = append(f.commits, srGit(t, f.origin, "rev-parse", "HEAD"))
	}
	srGit(t, f.origin, "tag", "-a", f.tags["stable"], f.commits[1], "-m", "stable")
	srGit(t, f.origin, "tag", "-a", f.tags["canary"], f.commits[2], "-m", "canary")
	srGit(t, f.origin, "tag", "v99.0.0", f.commits[3])
	srGit(t, f.origin, "tag", "v99.0.1+canary.20260912T125822Z", f.commits[3])
	srGit(t, tmp, "clone", f.origin, f.root)
	srGit(t, f.root, "checkout", "--detach", f.commits[0])
	repo := "/repos/" + OfficialRepository
	stable := map[string]any{"tag_name": f.tags["stable"], "draft": false, "prerelease": false}
	canary := map[string]any{"tag_name": f.tags["canary"], "draft": false, "prerelease": true}
	f.responses[repo+"/releases/latest"] = stable
	f.responses[repo+"/releases/tags/"+f.tags["stable"]] = stable
	f.responses[repo+"/releases/tags/"+f.tags["canary"]] = canary
	f.responses[repo+"/commits/"+f.tags["stable"]] = map[string]any{"sha": f.commits[1]}
	f.responses[repo+"/commits/"+f.tags["canary"]] = map[string]any{"sha": f.commits[2]}
	for channel, tag := range f.tags {
		f.responses["/releases/"+channel+"/index.html"] = `<meta name="icebath-build" content="` + tag + `">`
	}
	f.responses["/releases/stable/release-candidates.json"] = map[string]any{"tag": f.tags["stable"], "commit": f.commits[1]}
	srRoute(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if r.URL.RawQuery != "" {
			path += "?" + r.URL.RawQuery
		}
		f.mu.Lock()
		f.requests = append(f.requests, path)
		data, ok := f.responses[path]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		body, _ := json.Marshal(data)
		if text, isText := data.(string); isText {
			body = []byte(text)
		}
		if bom {
			body = append([]byte("\xef\xbb\xbf"), body...)
		}
		w.Write(body)
	}))
	return f
}

func srEachEncoding(t *testing.T, run func(t *testing.T, f *srReleases)) {
	for _, bom := range []bool{false, true} {
		name := "utf-8"
		if bom {
			name = "utf-8-sig"
		}
		t.Run(name, func(t *testing.T) { run(t, srFixture(t, bom)) })
	}
}

func TestSourceStableResolutionUsesPromotedPointerNotHighestTag(t *testing.T) {
	srEachEncoding(t, func(t *testing.T, f *srReleases) {
		for _, gitCmd := range [][]string{{"git"}, nil} {
			tag, sha := ResolveSourceRelease(context.Background(), "stable", gitCmd, f.root, "", true)
			if tag != f.tags["stable"] || sha != f.commits[1] {
				t.Fatalf("git %v resolved %q %q", gitCmd, tag, sha)
			}
		}
		if len(f.paths()) == 0 {
			t.Fatal("no release request was made")
		}
	})
}

func TestSourceForkOriginUsesItsOwnPublishedRelease(t *testing.T) {
	srEachEncoding(t, func(t *testing.T, f *srReleases) {
		url := "https://github.com/Fixture/icebath.git"
		srGit(t, f.root, "config", "remote.origin.url", url)
		srGit(t, f.root, "config", "url."+f.origin+".insteadOf", url)
		tag := f.tags["stable"]
		srGit(t, f.origin, "tag", "-f", tag, f.commits[3])
		f.set("/repos/Fixture/icebath/releases/latest", map[string]any{"tag_name": tag, "draft": false, "prerelease": false})
		f.set("/repos/Fixture/icebath/commits/"+tag, map[string]any{"sha": f.commits[3]})
		gotTag, sha := ResolveSourceRelease(context.Background(), "stable", []string{"git"}, f.root, "", true)
		if gotTag != tag || sha != f.commits[3] {
			t.Fatalf("resolved %q %q", gotTag, sha)
		}
		for _, path := range f.paths() {
			if !strings.HasPrefix(path, "/repos/Fixture/icebath/") {
				t.Fatalf("fork resolution asked %s", path)
			}
		}
	})
}

func TestSourceSelectedDraftNeverFallsBackToOtherTags(t *testing.T) {
	srEachEncoding(t, func(t *testing.T, f *srReleases) {
		f.set("/repos/"+OfficialRepository+"/releases/tags/"+f.tags["stable"], map[string]any{"tag_name": f.tags["stable"], "draft": true, "prerelease": false})
		for _, gitCmd := range [][]string{{"git"}, nil} {
			if tag, sha := ResolveSourceRelease(context.Background(), "stable", gitCmd, f.root, "", true); tag != "" || sha != "" {
				t.Fatalf("git %v resolved a draft to %q %q", gitCmd, tag, sha)
			}
		}
		for _, path := range f.paths() {
			if strings.Contains(path, "/tags?") {
				t.Fatalf("draft fell back through %s", path)
			}
		}
	})
}

func TestSourceOriginTagCannotSubstituteAForkCommit(t *testing.T) {
	srEachEncoding(t, func(t *testing.T, f *srReleases) {
		for i, channel := range []string{"stable", "canary"} {
			srGit(t, f.origin, "tag", "-f", f.tags[channel], f.commits[3])
			if tag, sha := ResolveSourceRelease(context.Background(), channel, []string{"git"}, f.root, "", true); tag != "" || sha != "" {
				t.Fatalf("%s accepted a moved origin tag %q %q", channel, tag, sha)
			}
			tag, sha := ResolveSourceRelease(context.Background(), channel, nil, "", "", true)
			if tag != f.tags[channel] || sha != f.commits[1+i] {
				t.Fatalf("%s without git resolved %q %q", channel, tag, sha)
			}
		}
	})
}

func TestSourceMissingPointersFallBackOnlyToPublishedReleases(t *testing.T) {
	srEachEncoding(t, func(t *testing.T, f *srReleases) {
		for i, channel := range []string{"stable", "canary"} {
			repo := "/repos/" + OfficialRepository
			f.drop("/releases/stable/release-candidates.json")
			f.drop("/releases/" + channel + "/index.html")
			published := map[string]any{"tag_name": f.tags[channel], "draft": false, "prerelease": channel == "canary"}
			f.set(repo+"/releases/latest", published)
			f.set(repo+"/releases?per_page=100&page=1", []any{
				map[string]any{"tag_name": "v99.0.1+canary.20260912T125822Z", "draft": true, "prerelease": true}, published})
			tag, sha := ResolveSourceRelease(context.Background(), channel, []string{"git"}, f.root, "", true)
			if tag != f.tags[channel] || sha != f.commits[1+i] {
				t.Fatalf("%s resolved %q %q", channel, tag, sha)
			}
			for _, path := range f.paths() {
				if strings.Contains(path, "/tags?") || path == repo+"/releases/tags/"+f.tags[channel] {
					t.Fatalf("%s asked %s", channel, path)
				}
			}
		}
	})
}

func TestSourceStableIsTheLatestPublishedGithubReleaseWithoutTheChannelTree(t *testing.T) {
	srEachEncoding(t, func(t *testing.T, f *srReleases) {
		saved := resolveChannel
		resolveChannel = func(context.Context, string, string) (*ChannelResolution, error) {
			t.Fatal("stable read a channel record")
			return nil, nil
		}
		t.Cleanup(func() { resolveChannel = saved })
		f.set("/releases/stable/release-candidates.json", map[string]any{"tag": "v99.0.0", "commit": f.commits[3]})
		target, err := ResolveSourceTarget(context.Background(), "stable", []string{"git"}, f.root, "", false)
		if err != nil || target.Commit != f.commits[1] || target.Version != "1.2.3" || target.Retired() {
			t.Fatalf("target %+v %v", target, err)
		}
		if got := target.Label(); got != "stable v1.2.3 ("+f.commits[1][:12]+")" {
			t.Fatalf("label %q", got)
		}
		for _, path := range f.paths() {
			if strings.HasPrefix(path, "/releases/") {
				t.Fatalf("stable read a channel record %s", path)
			}
		}
	})
}
