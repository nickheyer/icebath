package plugins

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const ucFeed = "version: 1.2.0\nreleased: 2026-09-03T00:00:00Z\nmin_hermes: 0.27.0\nartifacts:\n  git: https://example/o/r\n  bundle: https://example/o/r/plug-1.2.0.zip\n"

func ucGitProv(path string, row map[string]any) Provenance {
	full := map[string]any{"pinned": false, "revision": strings.Repeat("a", 40), "source": "https://example/o/r"}
	for k, v := range row {
		full[k] = v
	}
	if path == "" {
		path = "/x/plug"
	}
	return Provenance{Name: "plug", Class: ProvenanceGit, Path: path, Row: full}
}

func ucNo(t *testing.T) func(context.Context, string) (string, error) {
	return func(context.Context, string) (string, error) {
		t.Fatal("should not be called")
		return "", nil
	}
}

func ucConst(s string) func(context.Context, string) (string, error) {
	return func(context.Context, string) (string, error) { return s, nil }
}

func ucPlug(t *testing.T, manifest string) string {
	plug := filepath.Join(t.TempDir(), "plug")
	os.MkdirAll(plug, 0o755)
	if manifest != "" {
		os.WriteFile(filepath.Join(plug, "plugin.yaml"), []byte(manifest), 0o644)
	}
	return plug
}

func ucAvail(r *CheckResult) string {
	if r.UpdateAvailable == nil {
		return "unknown"
	}
	if *r.UpdateAvailable {
		return "true"
	}
	return "false"
}

func TestCheckProvenancedLocalAndFeed(t *testing.T) {
	ctx := context.Background()
	feedURL := map[string]any{"update_url": "https://feed.example/f.yml"}
	if r := checkProvenanced(ctx, Provenance{Name: "plug", Class: ProvenanceManual, Path: "/x/plug", Row: map[string]any{}}, ucNo(t), ucNo(t)); ucAvail(r) != "unknown" || !strings.Contains(r.Reason, "not auto-updatable") {
		t.Fatalf("manual %+v", r)
	}
	if r := checkProvenanced(ctx, Provenance{Name: "plug", Class: ProvenanceDrift, Path: "/x/plug", Row: map[string]any{"source": "https://x/y"}}, ucNo(t), ucNo(t)); !strings.Contains(r.Reason, "reinstall") {
		t.Fatalf("drift %+v", r)
	}
	if r := checkProvenanced(ctx, Provenance{Name: "plug", Class: ProvenanceSelfCloned, Path: "/x/plug", Row: map[string]any{}}, ucNo(t), ucNo(t)); !strings.Contains(r.Reason, "adopt") {
		t.Fatalf("self cloned %+v", r)
	}
	if r := checkProvenanced(ctx, ucGitProv("", map[string]any{"pinned": true}), ucNo(t), ucNo(t)); ucAvail(r) != "false" || !strings.Contains(r.Reason, "pinned") {
		t.Fatalf("pinned %+v", r)
	}
	plug := ucPlug(t, "name: plug\nupdate_url: https://evil.example/feed.yml\n")
	if r := checkProvenanced(ctx, ucGitProv(plug, nil), ucNo(t), ucNo(t)); !strings.Contains(r.NeedsFixing, "trust-update-url") || r.UpdateAvailable != nil {
		t.Fatalf("url appearing %+v", r)
	}
	plug = ucPlug(t, "name: plug\nversion: 1.0.0\nupdate_url: https://feed.example/f.yml\n")
	var fetched []string
	fetch := func(_ context.Context, u string) (string, error) { fetched = append(fetched, u); return ucFeed, nil }
	r := checkProvenanced(ctx, ucGitProv(plug, feedURL), fetch, ucNo(t))
	if strings.Join(fetched, ",") != "https://feed.example/f.yml" || r.Latest != "1.2.0" || r.Current != "1.0.0" || r.MinIcebath != "0.27.0" || ucAvail(r) != "true" {
		t.Fatalf("matching tag %+v", r)
	}
	plug = ucPlug(t, "name: plug\nupdate_url: https://feed.example/f.yml\n")
	if r := checkProvenanced(ctx, ucGitProv(plug, feedURL), ucConst(ucFeed), ucNo(t)); ucAvail(r) != "unknown" || r.Latest != "1.2.0" {
		t.Fatalf("versionless manifest %+v", r)
	}
	shaFeed := "version: 1.2.0\nartifacts:\n  git: " + strings.Repeat("c", 40) + "\n"
	plug = ucPlug(t, "name: plug\nversion: 9.9.9\nupdate_url: https://feed.example/f.yml\n")
	if r := checkProvenanced(ctx, ucGitProv(plug, feedURL), ucConst(shaFeed), ucNo(t)); ucAvail(r) != "true" || r.Current != strings.Repeat("a", 40) || r.Latest != strings.Repeat("c", 40) {
		t.Fatalf("sha to sha %+v", r)
	}
	c40 := map[string]any{"update_url": "https://feed.example/f.yml", "revision": strings.Repeat("c", 40)}
	if r := checkProvenanced(ctx, ucGitProv(plug, c40), ucConst(shaFeed), ucNo(t)); ucAvail(r) != "false" || r.Current != strings.Repeat("c", 40) {
		t.Fatalf("converged %+v", r)
	}
	if r := checkProvenanced(ctx, ucGitProv(plug, c40), ucConst(strings.ToUpper(shaFeed[:0])+"version: 1.2.0\nartifacts:\n  git: "+strings.Repeat("C", 40)+"\n"), ucNo(t)); ucAvail(r) != "false" {
		t.Fatalf("case equivalent %+v", r)
	}
	if r := checkProvenanced(ctx, ucGitProv(plug, map[string]any{"update_url": "https://feed.example/f.yml", "revision": ""}), ucConst(shaFeed), ucNo(t)); ucAvail(r) != "unknown" || !strings.Contains(r.Reason, "no full revision sha") {
		t.Fatalf("no revision %+v", r)
	}
	if r := checkProvenanced(ctx, ucGitProv(plug, map[string]any{"update_url": "https://feed.example/f.yml", "revision": "v1.2.0"}), ucConst(shaFeed), ucNo(t)); ucAvail(r) != "unknown" {
		t.Fatalf("tagged revision %+v", r)
	}
	plug = ucPlug(t, "update_url: https://feed.example/f.yml\n")
	if r := checkProvenanced(ctx, ucGitProv(plug, feedURL), ucConst("version: ["), ucNo(t)); ucAvail(r) != "unknown" || !strings.Contains(r.Reason, "unparseable") {
		t.Fatalf("malformed %+v", r)
	}
	if _, err := parseFeedYml("version: 1.2.0\nartifacts:\n  git:\n    url: x\n"); err == nil {
		t.Fatal("non string git artifact")
	}
	if _, err := parseFeedYml("released: 2026-01-01\n"); err == nil {
		t.Fatal("feed requires version")
	}
	if f, err := parseFeedYml("version: 2.0.0\n"); err != nil || f["version"] != "2.0.0" {
		t.Fatalf("%v %v", f, err)
	}
	plug = ucPlug(t, "name: plug\nupdate_url: https://feed.example/f.yml\n")
	boom := func(context.Context, string) (string, error) { return "", errors.New("timeout") }
	if r := checkProvenanced(ctx, ucGitProv(plug, feedURL), boom, ucNo(t)); !strings.HasPrefix(r.Reason, "feed fetch failed") || r.UpdateAvailable != nil {
		t.Fatalf("fetch failure %+v", r)
	}
	plug = ucPlug(t, "")
	var calls []string
	ls := func(_ context.Context, s string) (string, error) {
		calls = append(calls, s)
		return strings.Repeat("b", 40), nil
	}
	if r := checkProvenanced(ctx, ucGitProv(plug, nil), ucNo(t), ls); strings.Join(calls, ",") != "https://example/o/r" || ucAvail(r) != "true" {
		t.Fatalf("ls-remote fallback %+v", r)
	}
	versionBackup := RunningVersion
	RunningVersion = "0.30.0"
	defer func() { RunningVersion = versionBackup }()
	if got := heldForNewerIcebath(&CheckResult{MinIcebath: ">=999.0.0"}); got != "needs Icebath >=999.0.0; this is 0.30.0" {
		t.Fatalf("held %q", got)
	}
	if got := heldForNewerIcebath(&CheckResult{MinIcebath: "999.0.0"}); !strings.HasPrefix(got, "needs Icebath 999.0.0; this is ") {
		t.Fatalf("held %q", got)
	}
	if heldForNewerIcebath(&CheckResult{MinIcebath: "0.0.1"}) != "" || heldForNewerIcebath(&CheckResult{}) != "" {
		t.Fatal("an accepted floor is not held")
	}
}

func ucGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_DIR=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestLsRemoteLifecycleAgainstRealRepos(t *testing.T) {
	ctx := context.Background()
	backup := resolveGitExecutable
	t.Cleanup(func() { resolveGitExecutable = backup })
	resolveGitExecutable = func() string { gitPath, _ := exec.LookPath("git"); return gitPath }
	repo := filepath.Join(t.TempDir(), "repo")
	os.MkdirAll(repo, 0o755)
	ucGit(t, repo, "init", "-q", "-b", "main")
	ucGit(t, repo, "-c", "user.email=t@e", "-c", "user.name=t", "commit", "--allow-empty", "-qm", "one")
	sha1 := ucGit(t, repo, "rev-parse", "HEAD")
	plug := ucPlug(t, "")
	os.MkdirAll(filepath.Join(plug, ".git"), 0o755)
	check := func(rev string) *CheckResult {
		return checkProvenanced(ctx, ucGitProv(plug, map[string]any{"revision": rev, "source": repo}), ucNo(t), defaultLsRemote)
	}
	if r := check(sha1); ucAvail(r) != "false" {
		t.Fatalf("current %+v", r)
	}
	ucGit(t, repo, "-c", "user.email=t@e", "-c", "user.name=t", "commit", "--allow-empty", "-qm", "two")
	sha2 := ucGit(t, repo, "rev-parse", "HEAD")
	if r := check(sha1); ucAvail(r) != "true" || r.Latest != sha2 {
		t.Fatalf("available %+v", r)
	}
	if r := check(sha2); ucAvail(r) != "false" {
		t.Fatalf("applied %+v", r)
	}
	bare := filepath.Join(t.TempDir(), "bare.git")
	ucGit(t, t.TempDir(), "init", "--bare", "-q", bare)
	if head, err := defaultLsRemote(ctx, bare); err != nil || head != "" {
		t.Fatalf("bare %q %v", head, err)
	}
	if _, err := defaultLsRemote(ctx, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing source fails")
	}
}

func TestRunChecksNeverMutates(t *testing.T) {
	plugins := filepath.Join(t.TempDir(), "plugins")
	os.MkdirAll(filepath.Join(plugins, "plug", ".git"), 0o755)
	meta := filepath.Join(plugins, ".install-metadata.json")
	os.WriteFile(meta, []byte(`{"plug": {"pinned": false, "revision": "`+strings.Repeat("a", 40)+`", "source": "https://example/o/r"}}`), 0o644)
	before, _ := os.ReadFile(meta)
	p9NoGit(t)
	results := RunChecks(context.Background(), plugins, RunChecksOptions{Fetch: ucNo(t), LsRemote: ucConst(strings.Repeat("b", 40))})
	if ucAvail(results[0]) != "true" {
		t.Fatalf("%+v", results[0])
	}
	if after, _ := os.ReadFile(meta); string(after) != string(before) {
		t.Fatal("run checks mutated the sidecar")
	}
}

func TestDefaultFetchFollowsHTTPSRedirect(t *testing.T) {
	var visited []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		visited = append(visited, r.URL.Path)
		if r.URL.Path == "/secure" {
			http.Redirect(w, r, "/feed", http.StatusFound)
			return
		}
		w.Write([]byte("version: 1.2.0\n"))
	}))
	defer srv.Close()
	backup := feedTransport
	feedTransport = srv.Client().Transport
	defer func() { feedTransport = backup }()
	if got, err := defaultFetch(context.Background(), srv.URL+"/secure"); err != nil || got != "version: 1.2.0\n" {
		t.Fatalf("%q %v", got, err)
	}
	if strings.Join(visited, ",") != "/secure,/feed" {
		t.Fatalf("%v", visited)
	}
}
