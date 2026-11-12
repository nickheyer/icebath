package plugins

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Runs git in dir with a fixed identity and returns trimmed stdout, failing the test on error
func c6Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Records every tracked change and returns the new HEAD sha
func c6Commit(t *testing.T, repo, msg string) string {
	c6Git(t, repo, "add", "-A")
	c6Git(t, repo, "commit", "-q", "-m", msg)
	return c6Git(t, repo, "rev-parse", "HEAD")
}

func c6Write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Temp home with a plugins dir and install metadata records written verbatim
func c6Home(t *testing.T, metadata map[string]any) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.MkdirAll(home.PluginsDir(h), 0o755); err != nil {
		t.Fatal(err)
	}
	if metadata != nil {
		if err := writeInstallMetadata(context.Background(), h, metadata); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func c6RequireGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

func TestLoginBackendProvenanceVerdict(t *testing.T) {
	block := func(tier, pin string) map[string]any {
		return map[string]any{"name": "acme", "repo": "https://github.com/acme/acme", "tier": tier, "pin": pin, "sha": pin}
	}
	sha := strings.Repeat("a", 40)
	h := c6Home(t, map[string]any{
		"verified":  map[string]any{"source": "https://github.com/acme/acme", "catalog": block("verified", sha)},
		"official":  map[string]any{"source": "https://github.com/acme/acme", "catalog": block("official", sha)},
		"community": map[string]any{"source": "https://github.com/acme/acme", "catalog": block("community", sha)},
		"offpin":    map[string]any{"source": "https://github.com/acme/acme", "catalog": block("verified", "")},
		"removed":   map[string]any{"source": "https://github.com/acme/acme", "allow_removed": true, "catalog": block("verified", sha)},
		"urlonly":   map[string]any{"source": "https://github.com/acme/forged", "revision": sha},
	})
	dir := func(name string) string { return filepath.Join(home.PluginsDir(h), name) }
	c6Write(t, filepath.Join(dir("urlonly"), CatalogSidecar), `{"catalog_name": "acme", "sha": "`+sha+`", "tier": "verified"}`)
	cases := []struct {
		m    *Manifest
		want string
	}{
		{&Manifest{Source: "bundled", Path: "/x"}, ""},
		{&Manifest{Source: "user", Path: dir("verified")}, ""},
		{&Manifest{Source: "user", Path: dir("official")}, ""},
		{&Manifest{Source: "project", Path: "/p"}, "; this plugin was loaded from a project .icebath/plugins directory"},
		{&Manifest{Source: "entrypoint"}, "; this plugin was loaded from a pip entry point"},
		{&Manifest{Source: "user", Path: dir("community")}, "; catalog entry 'acme' is tier community"},
		{&Manifest{Source: "user", Path: dir("offpin")}, "; this install is not at the catalog's reviewed pin (installed with --ref)"},
		{&Manifest{Source: "user", Path: dir("removed")}, "; this plugin was installed with --allow-removed"},
		{&Manifest{Source: "user", Path: dir("urlonly")}, "; this plugin was not installed from the Icebath plugin catalog (a local directory or Git URL install)"},
		{&Manifest{Source: "user", Path: filepath.Join(t.TempDir(), "local")}, "; this plugin was not installed from the Icebath plugin catalog (a local directory or Git URL install)"},
	}
	for _, c := range cases {
		want := ""
		if c.want != "" {
			want = LoginBackendRule + c.want
		}
		if got := loginBackendProvenanceError(h, c.m); got != want {
			t.Errorf("%s %s: %q, want %q", c.m.Source, c.m.Path, got, want)
		}
		if c.want != "" {
			if got := browserPageBackendProvenanceError(h, c.m); got != BrowserPageBackendRule+c.want {
				t.Errorf("browser page %s: %q", c.m.Path, got)
			}
		}
	}
	if noErr(readCatalogSidecar(h, dir("urlonly"))) != nil {
		t.Error("an in-tree catalog file forged catalog provenance")
	}
}

func TestReadCatalogSidecarMigratesLegacyRecordFields(t *testing.T) {
	sha := strings.Repeat("B", 40)
	h := c6Home(t, map[string]any{"legacy": map[string]any{"source": "https://github.com/o/r#sub", "revision": sha,
		"pinned": true, "catalog_name": "legacy-entry", "catalog_tier": "verified", "installed_at": int64(7)}})
	target := filepath.Join(home.PluginsDir(h), "legacy")
	got := noErr(readCatalogSidecar(h, target))
	low := strings.ToLower(sha)
	want := map[string]any{"catalog_name": "legacy-entry", "repo": "https://github.com/o/r", "sha": low, "tier": "verified", "pin": low}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	metadata, err := readInstallMetadata(h)
	if err != nil {
		t.Fatal(err)
	}
	record := metadata["legacy"].(map[string]any)
	if _, ok := record["catalog_name"]; ok {
		t.Error("legacy catalog_name survived the migration")
	}
	if record["installed_at"] != int64(7) || record["catalog"].(map[string]any)["name"] != "legacy-entry" {
		t.Errorf("migrated record %v", record)
	}
	raw, _ := os.ReadFile(installMetadataPath(h))
	if !strings.Contains(string(raw), `"installed_at": 7,`) {
		t.Errorf("integer field lost its type on rewrite:\n%s", raw)
	}
	if !atCatalogPin(got, low) || atCatalogPin(got, "") || atCatalogPin(got, strings.Repeat("c", 40)) {
		t.Error("atCatalogPin verdicts wrong")
	}
	if a := noErr(catalogAnnotation(h, target)); a != "catalog:verified@"+low[:8] {
		t.Errorf("annotation %q", a)
	}
	row := noErr(CatalogRowFields(h, target, map[string]string{"legacy-entry": strings.Repeat("c", 40)}, map[string]string{"legacy-entry": "1.2"}))
	if row["update_available"] != true || row["catalog_version"] != "1.2" || row["catalog_tier"] != "verified" {
		t.Errorf("row fields %v", row)
	}
	if reason, ok := noErr2(removedAnnotation(h, "legacy", target, []RemovedEntry{{Name: "legacy-entry"}})); !ok || reason != "no reason recorded" {
		t.Errorf("removed annotation %q %v", reason, ok)
	}
}

func TestSurfaceDeltaAndConsentMessage(t *testing.T) {
	old := map[string]map[string]bool{"tools": {"a": true}, "desktop": {"plugin.js": true}}
	newSurface := map[string]map[string]bool{"tools": {"a": true, "c": true, "b": true}, "desktop": {"desktop/plugin.js": true},
		"python_dependencies": {"requests": true}}
	delta := surfaceDelta(old, newSurface)
	if len(delta) != 2 || !slices.Equal(delta["tools"], []string{"b", "c"}) || len(delta["desktop"]) != 0 {
		t.Fatalf("delta %v", delta)
	}
	err := &RepinConsentRequired{Name: "p", Sha: strings.Repeat("d", 40), Delta: delta}
	if err.Error() != "Updating 'p' to dddddddd adds tools: b, c; Python dependencies: requests. Confirm to continue." {
		t.Errorf("message %q", err.Error())
	}
	if d := surfaceDelta(map[string]map[string]bool{}, map[string]map[string]bool{"desktop": {"plugin.js": true}}); !slices.Equal(d["desktop"], []string{"plugin.js"}) {
		t.Errorf("new desktop half %v", d)
	}
	tree := t.TempDir()
	c6Write(t, filepath.Join(tree, "desktop", "plugin.js"), "x")
	if s := desktopSurface(tree); !s["desktop/plugin.js"] {
		t.Errorf("desktop surface %v", s)
	}
	prev := installedServices()
	t.Cleanup(func() { services.Store(prev) })
	InstallServices(&Services{})
	if _, err := pluginSurface(nil, tree); !errors.As(err, new(*ServiceMissingError)) {
		t.Errorf("missing OAuthSurface service: %v", err)
	}
}

func TestCarryUserFilesGitAndNoGitTrees(t *testing.T) {
	old, staged := t.TempDir(), t.TempDir()
	c6Write(t, filepath.Join(old, "data", "db.json"), "user")
	c6Write(t, filepath.Join(old, "notes.txt"), "mine")
	c6Write(t, filepath.Join(old, "plugin.py"), "old code")
	c6Write(t, filepath.Join(old, "plugin.yaml"), "name: x")
	c6Write(t, filepath.Join(old, "skills", "s.md"), "skill")
	c6Write(t, filepath.Join(old, ".venv", "lib.txt"), "artefact")
	c6Write(t, filepath.Join(staged, "notes.txt"), "upstream")
	carried, err := carryUserFiles(old, staged, []string{"data/", "notes.txt"}, true)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(carried)
	if !slices.Equal(carried, []string{"data/db.json", "notes.txt"}) {
		t.Errorf("git carried %v", carried)
	}
	if b, _ := os.ReadFile(filepath.Join(staged, "notes.txt")); string(b) != "mine" {
		t.Errorf("a user file in the untracked set must win, got %q", b)
	}
	staged2 := t.TempDir()
	c6Write(t, filepath.Join(staged2, "notes.txt"), "upstream")
	carried, err = carryUserFiles(old, staged2, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(carried, []string{"data/db.json"}) {
		t.Errorf("no-git carried %v", carried)
	}
	if b, _ := os.ReadFile(filepath.Join(staged2, "notes.txt")); string(b) != "upstream" {
		t.Errorf("no-git update must keep the new revision file, got %q", b)
	}
	if err := os.Symlink(filepath.Join(old, "notes.txt"), filepath.Join(old, "data", "link.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := carryUserFiles(old, t.TempDir(), []string{"data/"}, true); err == nil || !strings.Contains(err.Error(), "Cannot preserve symlinked user file(s) data/link.json") {
		t.Errorf("symlinked user file: %v", err)
	}
	clash := t.TempDir()
	c6Write(t, filepath.Join(clash, "notes.txt", "inner"), "dir now")
	if _, err := carryUserFiles(old, clash, []string{"notes.txt"}, true); err == nil || !strings.Contains(err.Error(), "the updated plugin now has a directory at that path") {
		t.Errorf("file to directory clash: %v", err)
	}
}

func TestGitURLAndRevisionHelpers(t *testing.T) {
	got, err := scrubGitURL("https://user:tok@github.com:8443/o/r.git?x=1#frag")
	if err != nil || got != "https://github.com:8443/o/r.git" {
		t.Errorf("scrub %q %v", got, err)
	}
	if got, _ := scrubGitURL("git@github.com:o/r.git"); got != "git@github.com:o/r.git" {
		t.Errorf("scp url changed %q", got)
	}
	if got, _ := canonicalSource("https://t@h.example/o/r", "sub/dir"); got != "https://h.example/o/r#sub/dir" {
		t.Errorf("canonical %q", got)
	}
	if _, err := normalizeExactRevision("abc"); err == nil {
		t.Error("short ref accepted")
	}
	if got, _ := normalizeExactRevision(strings.Repeat("A", 40)); got != strings.Repeat("a", 40) {
		t.Errorf("normalize %q", got)
	}
	if got := intentToAddPaths(" A new.txt\x00?? u.txt\x00A  staged\x00R  a\x00 A b c"); !slices.Equal(got, []string{"new.txt", "b c"}) {
		t.Errorf("intent to add %v", got)
	}
	if !LooksLikeCatalogName("my-plugin_2") || LooksLikeCatalogName("owner/repo") || LooksLikeCatalogName("Upper") || LooksLikeCatalogName("") {
		t.Error("catalog name shape verdicts wrong")
	}
	h := c6Home(t, map[string]any{"p": map[string]any{"pinned": true, "revision": strings.Repeat("e", 40)}})
	if a := pinAnnotation(h, "p", nil); a != "git pinned@eeeeeeee" {
		t.Errorf("pin annotation %q", a)
	}
}

func TestUpdateInstallRecordSerializesConcurrentWriters(t *testing.T) {
	h := c6Home(t, nil)
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := string(rune('a' + i))
			if err := updateInstallRecord(context.Background(), h, name, func(map[string]any) map[string]any {
				return map[string]any{"source": name}
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	metadata, err := readInstallMetadata(h)
	if err != nil || len(metadata) != 12 {
		t.Fatalf("records %d err %v", len(metadata), err)
	}
	ctx := context.Background()
	err = withInstallMetadataLock(ctx, h, func(inner context.Context) error {
		return updateInstallRecord(inner, h, "a", func(map[string]any) map[string]any { return nil })
	})
	if err != nil {
		t.Fatalf("reentrant update: %v", err)
	}
	if metadata, _ = readInstallMetadata(h); len(metadata) != 11 {
		t.Errorf("removal left %d records", len(metadata))
	}
}

func TestClonePullAndLocalChangesOverFileRepo(t *testing.T) {
	c6RequireGit(t)
	h := c6Home(t, nil)
	repo := filepath.Join(t.TempDir(), "repo")
	c6Write(t, filepath.Join(repo, "sub", "plugin.yaml"), "name: p\n")
	c6Write(t, filepath.Join(repo, "README"), "r\n")
	c6Write(t, filepath.Join(repo, ".gitignore"), "cache/\n")
	c6Git(t, repo, "init", "-q")
	sha1 := c6Commit(t, repo, "v1")
	c6Write(t, filepath.Join(repo, "sub", "plugin.yaml"), "name: p\nversion: 2\n")
	sha2 := c6Commit(t, repo, "v2")
	ctx := context.Background()
	url := "file://" + filepath.ToSlash(repo)

	pinned := filepath.Join(home.PluginsDir(h), "pinned")
	got, err := clonePluginRepo(ctx, h, pinned, url, sha1, "")
	if err != nil || got != sha1 {
		t.Fatalf("pinned clone %q %v", got, err)
	}
	sparse := filepath.Join(home.PluginsDir(h), "sparse")
	if got, err = clonePluginRepo(ctx, h, sparse, url, "", "sub"); err != nil || got != sha2 {
		t.Fatalf("subdir clone %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(sparse, "README")); err == nil {
		t.Error("sparse checkout materialized files outside the subdir")
	}
	if _, err := clonePluginRepo(ctx, h, filepath.Join(home.PluginsDir(h), "bad"), url, strings.Repeat("0", 40), ""); err == nil {
		t.Error("an unknown commit cloned")
	} else if pe := (*PluginOperationError)(nil); !errors.As(err, &pe) || pe.Class() != "clone_failed" {
		t.Errorf("unknown commit error %v", err)
	}

	tracking := filepath.Join(home.PluginsDir(h), "tracking")
	c6Git(t, filepath.Dir(tracking), "clone", "-q", url, tracking)
	c6Git(t, tracking, "reset", "-q", "--hard", sha1)
	c6Write(t, filepath.Join(tracking, "README"), "local edit\n")
	c6Write(t, filepath.Join(tracking, "cache", "c.bin"), "ignored")
	c6Write(t, filepath.Join(tracking, "mine.txt"), "untracked")
	local, modified, err := localChanges(ctx, tracking)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(local)
	if !slices.Equal(local, []string{"cache/", "mine.txt"}) || !slices.Equal(modified, []string{"README"}) {
		t.Errorf("local %v modified %v", local, modified)
	}
	if l, m, err := localChanges(ctx, sparse+"-missing"); l != nil || m != nil || err != nil {
		t.Errorf("no checkout: %v %v %v", l, m, err)
	}
	ok, msg, err := gitPullPluginDir(ctx, tracking)
	if err != nil || !ok || !strings.Contains(msg, "Local changes were re-applied on top of the update.") {
		t.Fatalf("pull %v %q %v", ok, msg, err)
	}
	if head := c6Git(t, tracking, "rev-parse", "HEAD"); head != sha2 {
		t.Errorf("pull left HEAD at %s", head)
	}
	if b, _ := os.ReadFile(filepath.Join(tracking, "README")); string(b) != "local edit\n" {
		t.Errorf("local edit lost: %q", b)
	}
	if ref, _ := stashRef(ctx, resolveGitExecutable(), tracking); ref != "" {
		t.Errorf("autostash entry left behind %s", ref)
	}
	var out bytes.Buffer
	console := &Console{Out: &out}
	if err := fail(console, "[red]Error:[/red] x"); !errors.As(err, new(*CommandExit)) || out.String() != "Error: x\n" {
		t.Errorf("fail printed %q returned %v", out.String(), err)
	}
}
