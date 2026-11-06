package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

type rlRelease struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Published  string `json:"published_at"`
	Assets     []any  `json:"assets"`
}

// Serves a GitHub releases API for repository with latest and a two page listing
func rlServer(t *testing.T, latest *rlRelease, listing []rlRelease) *httptest.Server {
	t.Helper()
	prefix := "/repos/" + OfficialRepository + "/releases"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "icebath-update" || r.Header.Get("Accept") != "application/vnd.github+json" {
			http.Error(w, "headers", http.StatusBadRequest)
			return
		}
		switch {
		case r.URL.Path == prefix+"/latest":
			if latest == nil {
				http.Error(w, `{"message": "Not Found"}`, http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(latest)
		case strings.HasPrefix(r.URL.Path, prefix+"/tags/"):
			tag := strings.TrimPrefix(r.URL.Path, prefix+"/tags/")
			for _, rel := range listing {
				if rel.Tag == tag {
					json.NewEncoder(w).Encode(rel)
					return
				}
			}
			http.Error(w, `{"message": "Not Found"}`, http.StatusNotFound)
		case r.URL.Path == prefix:
			half := len(listing) / 2
			if r.URL.Query().Get("page") == "2" {
				json.NewEncoder(w).Encode(listing[half:])
				return
			}
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?per_page=100&page=2>; rel="next"`, server.URL, prefix))
			json.NewEncoder(w).Encode(listing[:half])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	old := releaseAPIBase
	releaseAPIBase = server.URL
	t.Cleanup(func() { releaseAPIBase = old })
	t.Setenv("ICEBATH_GITHUB_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	return server
}

func rlBuild(t *testing.T, version string) {
	updaterecord.SetBuildVersion(version)
	t.Cleanup(func() { updaterecord.SetBuildVersion("") })
}

var rlListing = []rlRelease{
	{Tag: "v1.1.0", Published: "2026-10-03T00:00:00Z"},
	{Tag: "v1.2.0+canary.20261004T000000Z", Prerelease: true, Published: "2026-10-04T00:00:00Z"},
	{Tag: "v1.3.0+canary.20261006T000000Z", Draft: true, Prerelease: true, Published: "2026-10-06T00:00:00Z"},
	{Tag: "v1.0.0", Published: "2026-10-01T00:00:00Z"},
	{Tag: "v1.2.0-preview.2", Published: "2026-10-05T00:00:00Z"},
	{Tag: "v1.2.0-preview.10", Draft: true, Published: "2026-10-07T00:00:00Z"},
	{Tag: "v1.2.0-preview.1", Published: "2026-10-02T00:00:00Z"},
	{Tag: "v1.2.0+canary.20261003T000000Z", Prerelease: true, Published: "2026-10-03T00:00:00Z"},
}

func TestReleaseStableUsesPromotedLatestNotHighestTag(t *testing.T) {
	rlServer(t, &rlRelease{Tag: "v1.0.0", Published: "2026-10-01T00:00:00Z"}, rlListing)
	target, err := ResolveReleaseTarget(context.Background(), ChannelStable, false)
	if err != nil || target.Release.Tag != "v1.0.0" || target.Version != "1.0.0" || target.Ahead || target.Retired() {
		t.Fatalf("%+v %v", target, err)
	}
	if target.Label() != "stable v1.0.0" {
		t.Fatalf("label %q", target.Label())
	}
}

func TestReleaseSelectedDraftOrPrereleaseNeverFallsBack(t *testing.T) {
	for _, latest := range []rlRelease{{Tag: "v1.1.0", Draft: true}, {Tag: "v1.1.0", Prerelease: true}, {Tag: "v2026.7.20"}} {
		rlServer(t, &latest, rlListing)
		_, err := ResolveReleaseTarget(context.Background(), ChannelStable, false)
		if err == nil || err.Error() != latest.Tag+" is not a published stable release" {
			t.Fatalf("%+v: %v", latest, err)
		}
	}
}

func TestReleaseChannelsSelectOnlyPublishedMembers(t *testing.T) {
	rlServer(t, nil, rlListing)
	for channel, tag := range map[string]string{
		ChannelCanary: "v1.2.0+canary.20261004T000000Z",
		"preview":     "v1.2.0-preview.2",
		ChannelMain:   "v1.2.0+canary.20261004T000000Z",
	} {
		target, err := ResolveReleaseTarget(context.Background(), channel, false)
		if err != nil || target.Release.Tag != tag {
			t.Fatalf("%s: %+v %v", channel, target, err)
		}
	}
	if _, err := ResolveReleaseTarget(context.Background(), "nightly", false); err == nil || err.Error() != "No build published for channel nightly" {
		t.Fatalf("got %v", err)
	}
	if _, err := ResolveReleaseTarget(context.Background(), ChannelStable, false); err == nil || !strings.Contains(err.Error(), "returned error: 404") {
		t.Fatalf("missing latest gave %v", err)
	}
	if _, err := ResolveReleaseTarget(context.Background(), "Bad/Name", false); err == nil || err.Error() != "Invalid channel name" {
		t.Fatalf("got %v", err)
	}
}

func TestReleaseDefaultStableOnlyMovesForwardUntilChosen(t *testing.T) {
	rlServer(t, &rlRelease{Tag: "v1.0.0"}, rlListing)
	rlBuild(t, "v1.0.5")
	target, err := ResolveReleaseTarget(context.Background(), ChannelStable, true)
	if err != nil || !target.Ahead || !strings.Contains(target.Label(), "waits for the next release") {
		t.Fatalf("%+v %v", target, err)
	}
	if target, _ = ResolveReleaseTarget(context.Background(), ChannelStable, false); target.Ahead {
		t.Fatal("a chosen channel was pinned to the newer build")
	}
	rlBuild(t, "v1.0.0+canary.20261001T000000Z")
	if target, _ = ResolveReleaseTarget(context.Background(), ChannelStable, true); target.Ahead {
		t.Fatal("build metadata counted as newer")
	}
}

func TestReleaseNetworkStallBecomesANamedTimedOutFetch(t *testing.T) {
	if NetworkFetchTimeout != 300*time.Second {
		t.Fatalf("fetch bound %v", NetworkFetchTimeout)
	}
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer server.Close()
	defer close(release)
	old, oldTimeout := releaseAPIBase, releaseFetchTimeout
	releaseAPIBase, releaseFetchTimeout = server.URL, 300*time.Millisecond
	defer func() { releaseAPIBase, releaseFetchTimeout = old, oldTimeout }()
	_, err := ResolveReleaseTarget(context.Background(), ChannelStable, false)
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "fetch") {
		t.Fatalf("got %v", err)
	}
}

func TestReleaseRequestsCarryTokenAndRetryRejectedTokenAnonymously(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "" {
			http.Error(w, `{"message": "Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(rlRelease{Tag: "v1.0.0"})
	}))
	defer server.Close()
	old := releaseAPIBase
	releaseAPIBase = server.URL
	defer func() { releaseAPIBase = old }()
	t.Setenv("ICEBATH_GITHUB_TOKEN", " tok ")
	t.Setenv("GITHUB_TOKEN", "other")
	if _, err := ResolveReleaseTarget(context.Background(), ChannelStable, false); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "Bearer tok" || seen[1] != "" {
		t.Fatalf("authorization sequence %q", seen)
	}
}

func TestReleaseHTTPFailuresClassify(t *testing.T) {
	for status, body := range map[int]string{
		http.StatusForbidden:          `{"message": "API rate limit exceeded for 1.2.3.4."}`,
		http.StatusTooManyRequests:    `{}`,
		http.StatusBadGateway:         `oops`,
		http.StatusUnauthorized:       `{"message": "Requires authentication"}`,
		http.StatusServiceUnavailable: ``,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(body))
		}))
		old := releaseAPIBase
		releaseAPIBase = server.URL
		t.Setenv("ICEBATH_GITHUB_TOKEN", "")
		t.Setenv("GITHUB_TOKEN", "")
		_, err := ResolveReleaseTarget(context.Background(), ChannelStable, false)
		releaseAPIBase = old
		server.Close()
		want := map[int]string{403: "rate limiting", 429: "rate limiting", 502: "outage", 401: "anonymous fetch", 503: "outage"}[status]
		if err == nil || !strings.Contains(ClassifyFetchFailure(err.Error()), want) {
			t.Fatalf("%d: %v", status, err)
		}
	}
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachable.Close()
	old := releaseAPIBase
	releaseAPIBase = unreachable.URL
	defer func() { releaseAPIBase = old }()
	_, err := ResolveReleaseTarget(context.Background(), ChannelStable, false)
	if err == nil || !strings.HasPrefix(ClassifyFetchFailure(err.Error()), "✗ Network error") {
		t.Fatalf("got %v", err)
	}
}

func TestReleaseFetchFailureClassifier(t *testing.T) {
	cases := []struct{ detail, want, not string }{
		{"error: RPC failed; HTTP 429 curl 22 The requested URL returned error: 429", "try again in 5 minutes", ""},
		{"fatal: unable to access 'https://github.com/x/y.git/': The requested URL returned error: 429", "rate limiting", "Network error"},
		{"remote: API rate limit exceeded", "rate limiting", ""},
		{"error: RPC failed; HTTP 503 curl 22", "githubstatus.com", ""},
		{"fatal: unable to access 'https://github.com/x/y.git/': Could not resolve host: github.com", "✗ Network error", ""},
		{"fatal: could not read Username for 'https://github.com': terminal prompts disabled", "outage", "check your git credentials"},
		{"fatal: Authentication failed for 'https://github.com/x/y.git/'", "Authentication failed", ""},
		{"git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.", "https://github.com/nickheyer/icebath.git", ""},
		{"Host key verification failed.\nfatal: Could not read from remote repository.", "SSH authentication failed", ""},
	}
	for _, tc := range cases {
		msg := ClassifyFetchFailure(tc.detail)
		if !strings.Contains(msg, tc.want) || (tc.not != "" && strings.Contains(msg, tc.not)) {
			t.Fatalf("%q classified %q", tc.detail, msg)
		}
	}
	if msg := ClassifyFetchFailure("fatal: something odd"); msg != "✗ Failed to fetch updates from origin." {
		t.Fatalf("got %q", msg)
	}
	var out bytes.Buffer
	PrintFetchFailure(&out, "error: RPC failed; HTTP 429 curl 22\nfatal: expected flush after ref listing\n")
	if text := out.String(); !strings.Contains(text, "rate limiting") || !strings.Contains(text, "  error: RPC failed; HTTP 429") || !strings.Contains(text, "  fatal: expected flush after ref listing") {
		t.Fatalf("got %q", text)
	}
	out.Reset()
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	PrintFetchFailure(&out, strings.Join(lines, "\n"))
	if text := out.String(); !strings.Contains(text, "  line 14\n") || strings.Contains(text, "  line 15") || !strings.Contains(text, "  ... (5 more lines omitted)") {
		t.Fatalf("got %q", text)
	}
	out.Reset()
	PrintFetchFailure(&out, "  ")
	if out.String() != "✗ Failed to fetch updates from origin.\n" {
		t.Fatalf("got %q", out.String())
	}
}

func TestReleaseVersionPrecedence(t *testing.T) {
	ordered := []string{"v1.0.0-alpha", "v1.0.0-alpha.1", "v1.0.0-alpha.beta", "v1.0.0-beta", "v1.0.0-beta.2", "v1.0.0-beta.11", "v1.0.0-rc.1", "v1.0.0", "v1.0.1", "v1.10.0"}
	for i := 1; i < len(ordered); i++ {
		if CompareVersions(ordered[i-1], ordered[i]) != -1 || CompareVersions(ordered[i], ordered[i-1]) != 1 {
			t.Fatalf("%s vs %s", ordered[i-1], ordered[i])
		}
	}
	if CompareVersions("v1.2.3+canary.20260101T000000Z", "v1.2.3") != 0 || CompareVersions("git.abc", "v0.0.1") != -1 {
		t.Fatal("build metadata or unparseable version misordered")
	}
}

func TestReleaseLiveBinaryHashCheck(t *testing.T) {
	root := t.TempDir()
	name := "icebath"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	os.WriteFile(filepath.Join(root, name), []byte("release bytes"), 0o755)
	sum := sha256.Sum256([]byte("release bytes"))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	listing := []rlRelease{{Tag: "v1.0.0", Assets: []any{map[string]any{"name": hostAssetName(), "digest": digest, "size": 13}}}}
	rlServer(t, nil, listing)
	rlBuild(t, "v1.0.0")
	if change, err := LocalBinaryChanges(context.Background(), root); err != nil || change != nil {
		t.Fatalf("%+v %v", change, err)
	}
	os.WriteFile(filepath.Join(root, name), []byte("rebuilt locally"), 0o755)
	change, err := LocalBinaryChanges(context.Background(), root)
	if err != nil || change == nil || change.Expected != hex.EncodeToString(sum[:]) || change.Release.Tag != "v1.0.0" {
		t.Fatalf("%+v %v", change, err)
	}
	rlBuild(t, "v1.0.0-3-gabcdef1")
	if _, err := LocalBinaryChanges(context.Background(), root); err == nil {
		t.Fatal("a describe build verified against a release")
	}
}
