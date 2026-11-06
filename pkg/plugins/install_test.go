package plugins

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Plugin repo with an old and a new commit of a demo manifest
func c7PluginRepo(t *testing.T, manifest string) (string, string, string) {
	t.Helper()
	c6RequireGit(t)
	repo := filepath.Join(t.TempDir(), "repo")
	c6Write(t, filepath.Join(repo, "plugin.yaml"), manifest)
	c6Git(t, repo, "init", "-q")
	c6Write(t, filepath.Join(repo, "marker.txt"), "old")
	old := c6Commit(t, repo, "old")
	c6Write(t, filepath.Join(repo, "marker.txt"), "new")
	return repo, old, c6Commit(t, repo, "new")
}

func c7NoInstallState(t *testing.T, h string) {
	t.Helper()
	for _, rel := range []string{"demo", ".install-metadata.json"} {
		if _, err := os.Stat(filepath.Join(home.PluginsDir(h), rel)); err == nil {
			t.Fatalf("%s exists after a refused install", rel)
		}
	}
}

func TestResolveGitURLForms(t *testing.T) {
	cases := []struct{ in, url, subdir string }{
		{"owner/repo", "https://github.com/owner/repo.git", ""},
		{"owner/repo/path/to/plugin", "https://github.com/owner/repo.git", "path/to/plugin"},
		{"owner/repo#sub/dir", "https://github.com/owner/repo.git", "sub/dir"},
		{"https://github.com/o/r/tree/main/plugins/x", "https://github.com/o/r.git", "plugins/x"},
		{"https://github.com/o/r.git/blob/main/README.md", "https://github.com/o/r.git", ""},
		{"https://example.com/r.git#sub/", "https://example.com/r.git", "sub"},
		{"https://example.com/r.git/sub/dir", "https://example.com/r.git", "sub/dir"},
		{"file:///tmp/repo", "file:///tmp/repo", ""},
	}
	for _, c := range cases {
		url, subdir, err := resolveGitURL(c.in)
		if err != nil || url != c.url || subdir != c.subdir {
			t.Errorf("%s: got %q %q %v", c.in, url, subdir, err)
		}
	}
	if _, _, err := resolveGitURL("justaname"); err == nil || !strings.Contains(err.Error(), "Invalid plugin identifier: 'justaname'") {
		t.Fatalf("bare name: %v", err)
	}
	if got := repoNameFromURL("git@github.com:owner/repo.git"); got != "repo" {
		t.Fatalf("ssh repo name %q", got)
	}
}

func TestResolveSubdirWithinRefusesEscapes(t *testing.T) {
	root := t.TempDir()
	c6Write(t, filepath.Join(root, "file"), "x")
	os.MkdirAll(filepath.Join(root, "ok"), 0o755)
	os.Symlink(t.TempDir(), filepath.Join(root, "out"))
	for sub, want := range map[string]string{"../x": "escapes", "out": "escapes", "missing": "does not exist", "file": "is not a directory"} {
		_, err := resolveSubdirWithin(root, sub)
		var opErr *PluginOperationError
		if !errors.As(err, &opErr) || opErr.FailureClass != "invalid_source" || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", sub, err)
		}
	}
	if got, err := resolveSubdirWithin(root, "ok"); err != nil || got != filepath.Join(resolvePath(root), "ok") {
		t.Fatalf("ok: %q %v", got, err)
	}
}

func TestInvalidRefIsRejectedBeforeAnyInstallState(t *testing.T) {
	repo, _, _ := c7PluginRepo(t, "name: demo\nversion: 1.0.0\n")
	h := c6Home(t, nil)
	ctx := home.WithOverride(context.Background(), h)
	for _, ref := range []string{"main", "abc", strings.Repeat("g", 40), strings.Repeat("a", 39), strings.Repeat("a", 41)} {
		_, _, _, err := installPluginCore(ctx, "file://"+repo, installCoreOptions{Ref: ref})
		if err == nil || !strings.Contains(err.Error(), "40-character commit SHA") {
			t.Fatalf("%q: %v", ref, err)
		}
		c7NoInstallState(t, h)
	}
}

func TestInstallRefusesANonHTTPSUpdateURL(t *testing.T) {
	for _, feed := range []string{"http://feed.example/f.yml", "file:///etc/feed.yml", "ftp://feed.example/f.yml"} {
		repo, _, _ := c7PluginRepo(t, "name: demo\nversion: 1.0.0\nupdate_url: "+feed+"\n")
		h := c6Home(t, nil)
		installedServicesBackup := installedServices()
		InstallServices(&Services{})
		_, _, _, err := installPluginCore(home.WithOverride(context.Background(), h), "file://"+repo, installCoreOptions{})
		InstallServices(installedServicesBackup)
		var opErr *PluginOperationError
		if !errors.As(err, &opErr) || opErr.FailureClass != "manifest_invalid" || !strings.Contains(err.Error(), "https://") {
			t.Fatalf("%s: %v", feed, err)
		}
		c7NoInstallState(t, h)
	}
}

func TestInspectedShaRefusesAMovedSource(t *testing.T) {
	repo, old, head := c7PluginRepo(t, "name: demo\n")
	h := c6Home(t, nil)
	ctx := home.WithOverride(context.Background(), h)
	_, err := cloneAtReviewedCommit(ctx, h, filepath.Join(t.TempDir(), "clone"), "file://"+repo, "", old, "", false)
	var moved *PluginSourceMoved
	if !errors.As(err, &moved) || moved.Class() != "clone_failed" || !strings.Contains(err.Error(), "now at commit "+head[:12]+", not "+old[:12]) {
		t.Fatalf("moved: %v", err)
	}
	var opErr *PluginOperationError
	if !errors.As(err, &opErr) {
		t.Fatal("a moved source is an operation error")
	}
	_, err = cloneAtReviewedCommit(ctx, h, filepath.Join(t.TempDir(), "clone"), "file://"+repo, head, old, "", false)
	if !errors.As(err, &opErr) || opErr.FailureClass != "already_installed" || !strings.Contains(err.Error(), "pinned to commit "+head[:12]) {
		t.Fatalf("retained pin: %v", err)
	}
	got, err := cloneAtReviewedCommit(ctx, h, filepath.Join(t.TempDir(), "clone"), "file://"+repo, "", strings.ToUpper(head), "", false)
	if err != nil || got != head {
		t.Fatalf("reviewed head: %q %v", got, err)
	}
}

func TestPinnedPluginUpdateRefusesToDrift(t *testing.T) {
	h := c6Home(t, map[string]any{"demo": map[string]any{"pinned": true, "revision": strings.Repeat("a", 40), "source": "file:///x"}})
	target := filepath.Join(home.PluginsDir(h), "demo")
	os.MkdirAll(filepath.Join(target, ".git"), 0o755)
	_, err := updatePlugin(home.WithOverride(context.Background(), h), target, updatePluginOptions{})
	if err == nil || err.Error() != "Plugin 'demo' is pinned; reinstall with an explicit --ref to change it." {
		t.Fatalf("pinned: %v", err)
	}
	h = c6Home(t, map[string]any{"demo": map[string]any{"source": "file:///x"}})
	target = filepath.Join(home.PluginsDir(h), "demo")
	os.MkdirAll(target, 0o755)
	_, err = updatePlugin(home.WithOverride(context.Background(), h), target, updatePluginOptions{})
	if err == nil || !strings.Contains(err.Error(), "has no owned Git checkout") {
		t.Fatalf("no checkout: %v", err)
	}
}

func TestFeedParsingAndHTTPSOnlyFetch(t *testing.T) {
	feed, err := parseFeedYml("version: ' 1.2 '\nmin_hermes: '0.9'\nnotes_url: ''\nartifacts:\n  git: abc\n  bundle: 3\n")
	want := map[string]any{"version": "1.2", "min_icebath": "0.9", "artifacts": map[string]any{"git": "abc"}}
	if err != nil || !reflect.DeepEqual(feed, want) {
		t.Fatalf("feed %v %v", feed, err)
	}
	for text, msg := range map[string]string{"- a": "feed must be a YAML mapping", "version: 1": "feed missing 'version'",
		"version: '1'\nartifacts:\n  git: 3": "feed artifacts.git must be a string", "a: [": "invalid feed YAML: "} {
		if _, err := parseFeedYml(text); err == nil || !strings.HasPrefix(err.Error(), msg) {
			t.Errorf("%q: %v", text, err)
		}
	}
	if got, err := httpsUpdateURL(" https://x/f.yml "); err != nil || got != "https://x/f.yml" {
		t.Fatalf("https: %q %v", got, err)
	}
	if _, err := httpsUpdateURL("http://x"); err == nil || err.Error() != "update_url must be an https:// URL, got 'http://x'" {
		t.Fatalf("http: %v", err)
	}
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("version: '1'")) }))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/f.yml", http.StatusFound)
	}))
	defer secure.Close()
	if _, err := defaultFetch(context.Background(), plain.URL); err == nil || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("plain fetch: %v", err)
	}
	if _, err := defaultFetch(context.Background(), secure.URL); err == nil {
		t.Fatal("a redirect to http must be refused")
	}
}

func TestProvenanceClassesAndLocalCheck(t *testing.T) {
	h := c6Home(t, map[string]any{
		"git":    map[string]any{"source": "s", "revision": strings.Repeat("b", 40), "update_url": "https://a/f.yml"},
		"drift":  map[string]any{"source": "s"},
		"pinned": map[string]any{"source": "s", "pinned": true, "revision": strings.Repeat("c", 40)},
	})
	dir := home.PluginsDir(h)
	for _, name := range []string{"git", "pinned", "self", "manual", "drift", ".hidden"} {
		os.MkdirAll(filepath.Join(dir, name), 0o755)
	}
	for _, name := range []string{"git", "pinned", "self"} {
		os.MkdirAll(filepath.Join(dir, name, ".git"), 0o755)
	}
	c6Write(t, filepath.Join(dir, "self", ".git", "config"), "[core]\n\turl = no\n[remote \"origin\"]\n\turl = https://example.com/self.git\n")
	c6Write(t, filepath.Join(dir, "git", "plugin.yaml"), "update_url: https://b/f.yml\n")
	resolveBackup := resolveGitExecutable
	resolveGitExecutable = func() string { return "" }
	defer func() { resolveGitExecutable = resolveBackup }()
	got := map[string]Provenance{}
	var order []string
	for _, p := range PluginsProvenance(context.Background(), dir) {
		got[p.Name] = p
		order = append(order, p.Name)
	}
	if strings.Join(order, ",") != "drift,git,manual,pinned,self" {
		t.Fatalf("order %v", order)
	}
	if got["self"].Class != ProvenanceSelfCloned || got["self"].OriginURL != "https://example.com/self.git" ||
		got["drift"].Class != ProvenanceDrift || got["manual"].Class != ProvenanceManual || got["git"].SavedUpdateURL != "https://a/f.yml" {
		t.Fatalf("classes %+v", got)
	}
	if r := checkLocalProvenance(got["git"]); r.NeedsFixing != "update_url mismatch: saved 'https://a/f.yml', manifest declares 'https://b/f.yml'; run `icebath plugins trust-update-url` after review" {
		t.Fatalf("mismatch %q", r.NeedsFixing)
	}
	if r := checkLocalProvenance(got["pinned"]); r.Reason != "pinned @ cccccccccccc" || r.UpdateAvailable == nil || *r.UpdateAvailable {
		t.Fatalf("pinned %+v", r)
	}
	if r := checkLocalProvenance(got["drift"]); !strings.Contains(r.Reason, "sidecar records 's' but the dir has no .git") {
		t.Fatalf("drift %q", r.Reason)
	}
	if v := versionIsNewer("1.10", "1.9"); v == nil || !*v {
		t.Fatal("1.10 is newer than 1.9")
	}
	if versionIsNewer("not a version", "1") != nil {
		t.Fatal("an invalid version is unknown")
	}
}

func TestScanBlockedResultAndPreservedFilesNote(t *testing.T) {
	result := &skills.ScanResult{Verdict: "caution", Findings: []skills.Finding{
		{PatternID: "p", Severity: "high", Category: "c", File: "user.py", Line: 3, Description: "d"},
		{PatternID: "q", Severity: "low", Category: "c", File: "up.py", Line: 1, Description: "e"},
	}}
	blocked := newPluginScanBlocked("blocked", result)
	if blocked.Class() != "scan_blocked" {
		t.Fatal(blocked.Class())
	}
	got := ScanBlockedResult(blocked)
	if got["scan_verdict"] != "caution" || len(got["scan_findings"].([]any)) != 2 || got["error"] != "blocked" {
		t.Fatalf("%v", got)
	}
	if note := preservedFilesNote(blocked, []string{"user.py"}); note != "blocked\n\nThese findings come from user files preserved from the installed copy: user.py. Move or remove them and retry the update." {
		t.Fatalf("note %q", note)
	}
	if note := preservedFilesNote(blocked, []string{"other"}); !strings.HasSuffix(note, "The scanned tree included user files preserved from the installed copy.") {
		t.Fatalf("note %q", note)
	}
	if ScanBlockedResult(newPluginScanBlocked("x", nil))["scan_verdict"] != "dangerous" {
		t.Fatal("no result reads as dangerous")
	}
}

func TestPublishFailureClassAndSharedAnswers(t *testing.T) {
	cases := map[string]error{
		"non_interactive":  &PluginOperationError{Msg: "x", FailureClass: "non_interactive"},
		"permission":       &fs.PathError{Op: "open", Path: "p", Err: syscall.EACCES},
		"filesystem_error": &fs.PathError{Op: "open", Path: "p", Err: syscall.ENOSPC},
		"deps_failed":      errors.New("resolver conflict"),
	}
	for want, err := range cases {
		if got := publishFailureClass(err); got != want {
			t.Errorf("%v: %s", err, got)
		}
	}
	if got := publishFailureClass(&PluginOperationError{Msg: "x", FailureClass: "other"}); got != "deps_failed" {
		t.Fatal(got)
	}
	asked := 0
	backup := inputLine
	inputLine = func(string) (string, error) { asked++; return " Yes ", nil }
	defer func() { inputLine = backup }()
	var out strings.Builder
	console := NewConsole(&out)
	ctx := WithSharedDependencyAnswers(context.Background())
	if !askYesNo(ctx, []any{"python", "p"}, "? ", console) || !askYesNo(ctx, []any{"python", "p"}, "? ", console) || asked != 1 {
		t.Fatalf("shared answers asked %d", asked)
	}
	if !strings.Contains(out.String(), "Reusing your answer for the first profile: yes") {
		t.Fatal(out.String())
	}
	askYesNo(context.Background(), []any{"python", "p"}, "? ", console)
	if asked != 2 {
		t.Fatal("without shared answers each question is asked")
	}
}

func TestCopyExampleFilesAndMissingEnv(t *testing.T) {
	h := c6Home(t, nil)
	dir := t.TempDir()
	c6Write(t, filepath.Join(dir, "config.yaml.example"), "a")
	c6Write(t, filepath.Join(dir, "keep.example"), "new")
	c6Write(t, filepath.Join(dir, "keep"), "mine")
	var out strings.Builder
	copyExampleFiles(dir, NewConsole(&out))
	if b, _ := os.ReadFile(filepath.Join(dir, "config.yaml")); string(b) != "a" {
		t.Fatal("example not copied")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "keep")); string(b) != "mine" {
		t.Fatal("existing file overwritten")
	}
	if out.String() != "  Created config.yaml from config.yaml.example\n" {
		t.Fatalf("%q", out.String())
	}
	t.Setenv("C7_SET", "1")
	missing, err := missingEnvSpecs(context.Background(), h, map[string]any{"requires_env": []any{"C7_SET", "C7_UNSET",
		map[string]any{"name": "C7_DICT", "secret": true}, map[string]any{"description": "nameless"}, 5}})
	if err != nil || len(missing) != 2 || missing[0]["name"] != "C7_UNSET" || missing[1]["name"] != "C7_DICT" {
		t.Fatalf("%v %v", missing, err)
	}
}
