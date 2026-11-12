package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Publication home with no plugin selection, as the offline worker fixture of the install ref cases leaves it
func refHome(t *testing.T) (string, context.Context) {
	t.Helper()
	_, h := pubWorld(t)
	if err := os.Remove(filepath.Join(h, "config.yaml")); err != nil {
		t.Fatal(err)
	}
	return h, home.WithOverride(context.Background(), h)
}

// Install metadata file of the home as raw JSON
func refMetadata(t *testing.T, h string) map[string]any {
	t.Helper()
	var metadata map[string]any
	if err := json.Unmarshal([]byte(gitPullRead(t, filepath.Join(h, "plugins", ".install-metadata.json"))), &metadata); err != nil {
		t.Fatal(err)
	}
	return metadata
}

func refEntry(t *testing.T, h, name string) map[string]any {
	t.Helper()
	entry, _ := refMetadata(t, h)[name].(map[string]any)
	return entry
}

func refURI(path string) string { return "file://" + filepath.ToSlash(path) }

// Writes the marker, commits every change and returns the new HEAD
func refCommit(t *testing.T, repo, message, marker string) string {
	t.Helper()
	c6Write(t, filepath.Join(repo, "marker.txt"), marker)
	return c6Commit(t, repo, message)
}

func TestExactRefInstallsOldCommitAndNormalizesUppercase(t *testing.T) {
	h, ctx := refHome(t)
	repo, old, newer := c7PluginRepo(t, "name: demo\nversion: 1.0.0\n")
	target, _, name, err := installPluginCore(ctx, refURI(repo), installCoreOptions{Ref: strings.ToUpper(old)})
	if err != nil {
		t.Fatal(err)
	}
	if name != "demo" || c6Git(t, target, "rev-parse", "HEAD") != old || old == newer || gitPullRead(t, filepath.Join(target, "marker.txt")) != "old" {
		t.Fatalf("install %s %s", name, target)
	}
	want := map[string]any{"demo": map[string]any{"pinned": true, "revision": old, "source": refURI(repo)}}
	if got := refMetadata(t, h); !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata %v", got)
	}
}

// Monorepo with one plugin under the subdirectory, committed twice with value.txt old then new
func refMonorepo(t *testing.T, sub string) (repo, old string) {
	t.Helper()
	c6RequireGit(t)
	repo = filepath.Join(t.TempDir(), "monorepo")
	plugin := filepath.Join(repo, filepath.FromSlash(sub))
	c6Write(t, filepath.Join(plugin, "plugin.yaml"), "name: nested-demo\n")
	c6Write(t, filepath.Join(plugin, "value.txt"), "old")
	c6Git(t, repo, "init", "-q")
	old = c6Commit(t, repo, "old")
	c6Write(t, filepath.Join(plugin, "value.txt"), "new")
	c6Commit(t, repo, "new")
	return repo, old
}

func TestSubdirPinRecordsSourceIdentityAndInstallsRequestedTree(t *testing.T) {
	h, ctx := refHome(t)
	repo, old := refMonorepo(t, "extensions/demo")
	identifier := refURI(repo) + "#extensions/demo"
	target, _, _, err := installPluginCore(ctx, identifier, installCoreOptions{Ref: old})
	if err != nil {
		t.Fatal(err)
	}
	if got := gitPullRead(t, filepath.Join(target, "value.txt")); got != "old" {
		t.Fatalf("value %q", got)
	}
	want := map[string]any{"pinned": true, "revision": old, "source": identifier}
	if got := refEntry(t, h, "nested-demo"); !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata %v", got)
	}
}

func TestCloneTimeoutAppliesToEveryNetworkStepOfAPinnedInstall(t *testing.T) {
	h, ctx := refHome(t)
	c6Write(t, filepath.Join(h, "config.yaml"), "plugins:\n  clone_timeout_seconds: 137\n")
	repo, old, _ := c7PluginRepo(t, "name: demo\nversion: 1.0.0\n")
	type call struct {
		step    string
		timeout time.Duration
	}
	calls := []call{}
	saved := runPluginGit
	t.Cleanup(func() { runPluginGit = saved })
	runPluginGit = func(ctx context.Context, gitExe, target string, timeout time.Duration, authURL string, args ...string) (*gitResult, error) {
		calls = append(calls, call{args[0], timeout})
		return saved(ctx, gitExe, target, timeout, authURL, args...)
	}
	target, _, _, err := installPluginCore(ctx, refURI(repo), installCoreOptions{Ref: old})
	if err != nil {
		t.Fatal(err)
	}
	if c6Git(t, target, "rev-parse", "HEAD") != old {
		t.Fatal("pinned install is not at the old commit")
	}
	for _, step := range []string{"clone", "fetch", "checkout"} {
		if !slices.Contains(calls, call{step, 137 * time.Second}) {
			t.Fatalf("no %s at 137s in %v", step, calls)
		}
	}
}

func TestSubdirInstallDownloadsOnlyThatSubdirectory(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(map[bool]string{false: "unpinned", true: "pinned"}[pinned], func(t *testing.T) {
			_, ctx := refHome(t)
			c6RequireGit(t)
			repo := filepath.Join(t.TempDir(), "monorepo")
			c6Write(t, filepath.Join(repo, "integrations", "icebath", "plugin.yaml"), "name: nested-demo\n")
			c6Write(t, filepath.Join(repo, "unrelated.bin"), strings.Repeat("x", 4096))
			c6Git(t, repo, "init", "-q")
			c6Git(t, repo, "config", "uploadpack.allowFilter", "true")
			sha := c6Commit(t, repo, "init")
			unrelated := c6Git(t, repo, "rev-parse", "HEAD:unrelated.bin")
			var worktree []string
			missing := ""
			inspect := func(_ map[string]any, tree string) ([]string, error) {
				clone := filepath.Dir(filepath.Dir(tree))
				entries, err := os.ReadDir(clone)
				if err != nil {
					return nil, err
				}
				for _, e := range entries {
					if e.Name() != ".git" {
						worktree = append(worktree, e.Name())
					}
				}
				slices.Sort(worktree)
				missing = c6Git(t, clone, "rev-list", "--objects", "--missing=print", "HEAD")
				return nil, nil
			}
			ref := ""
			if pinned {
				ref = sha
			}
			target, _, _, err := installPluginCore(ctx, refURI(repo)+"#integrations/icebath", installCoreOptions{Ref: ref, BeforeSwap: inspect})
			if err != nil {
				t.Fatal(err)
			}
			if !isFile(filepath.Join(target, "plugin.yaml")) || !reflect.DeepEqual(worktree, []string{"integrations"}) ||
				!slices.Contains(strings.Split(missing, "\n"), "?"+unrelated) {
				t.Fatalf("worktree %v missing %q", worktree, missing)
			}
		})
	}
}

func TestForceReinstallDoesNotDriftPinWithoutExplicitNewRef(t *testing.T) {
	h, ctx := refHome(t)
	repo, old, newer := c7PluginRepo(t, "name: demo\nversion: 1.0.0\n")
	if _, _, _, err := installPluginCore(ctx, refURI(repo), installCoreOptions{Ref: old}); err != nil {
		t.Fatal(err)
	}
	target, _, _, err := installPluginCore(ctx, refURI(repo), installCoreOptions{Force: true})
	if err != nil || c6Git(t, target, "rev-parse", "HEAD") != old || refEntry(t, h, "demo")["pinned"] != true {
		t.Fatalf("force reinstall drifted %v %v", refEntry(t, h, "demo"), err)
	}
	target, _, _, err = installPluginCore(ctx, refURI(repo), installCoreOptions{Force: true, Ref: newer})
	if err != nil || c6Git(t, target, "rev-parse", "HEAD") != newer || refEntry(t, h, "demo")["revision"] != newer {
		t.Fatalf("force reinstall at the new ref %v %v", refEntry(t, h, "demo"), err)
	}
}

func TestForceReinstallOfTheSameSourceKeepsTheUsersFiles(t *testing.T) {
	for _, move := range []string{"pinned-new-ref", "unpinned-reinstall", "other-source"} {
		t.Run(move, func(t *testing.T) {
			h, ctx := refHome(t)
			repo, _, _ := c7PluginRepo(t, "name: demo\nversion: 1.0.0\n")
			c6Write(t, filepath.Join(repo, ".gitignore"), "data/\n")
			old := refCommit(t, repo, "ignore data", "old")
			ref := ""
			if move == "pinned-new-ref" {
				ref = old
			}
			target, _, _, err := installPluginCore(ctx, refURI(repo), installCoreOptions{Ref: ref})
			if err != nil {
				t.Fatal(err)
			}
			c6Write(t, filepath.Join(target, "data", "state.json"), `{"user": 1}`)
			c6Write(t, filepath.Join(target, "settings.local.yaml"), "mode: mine\n")
			c6Write(t, filepath.Join(target, "marker.txt"), "edited")
			newSHA := refCommit(t, repo, "newer", "newer")
			if move == "other-source" {
				repo, _, newSHA = c7PluginRepo(t, "name: demo\nversion: 1.0.0\n")
			}
			ref = ""
			if move == "pinned-new-ref" {
				ref = newSHA
			}
			target, _, _, err = installPluginCore(ctx, refURI(repo), installCoreOptions{Force: true, Ref: ref})
			if err != nil {
				t.Fatal(err)
			}
			kept := move != "other-source"
			marker := map[bool]string{true: "newer", false: "new"}[kept]
			if c6Git(t, target, "rev-parse", "HEAD") != newSHA || gitPullRead(t, filepath.Join(target, "marker.txt")) != marker {
				t.Fatalf("reinstalled tree at %s", c6Git(t, target, "rev-parse", "HEAD"))
			}
			if isFile(filepath.Join(target, "data", "state.json")) != kept || isFile(filepath.Join(target, "settings.local.yaml")) != kept {
				t.Fatalf("user files kept %v", !kept)
			}
			backups, _ := filepath.Glob(filepath.Join(h, "plugins-backup", "demo-*"))
			markers := []string{}
			for _, b := range backups {
				markers = append(markers, gitPullRead(t, filepath.Join(b, "marker.txt")))
			}
			if want := map[bool][]string{true: {"edited"}, false: {}}[kept]; !reflect.DeepEqual(markers, want) {
				t.Fatalf("backups %v", markers)
			}
			if kept && (gitPullRead(t, filepath.Join(target, "data", "state.json")) != `{"user": 1}` ||
				gitPullRead(t, filepath.Join(target, "settings.local.yaml")) != "mode: mine\n") {
				t.Fatal("user files changed")
			}
		})
	}
}

func TestReinstallAfterManualDirectoryRemovalRetainsPin(t *testing.T) {
	h, ctx := refHome(t)
	repo, old, _ := c7PluginRepo(t, "name: demo\nversion: 1.0.0\n")
	target, _, _, err := installPluginCore(ctx, refURI(repo), installCoreOptions{Ref: old})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	target, _, _, err = installPluginCore(ctx, refURI(repo), installCoreOptions{})
	if err != nil || c6Git(t, target, "rev-parse", "HEAD") != old || refEntry(t, h, "demo")["pinned"] != true {
		t.Fatalf("reinstall after removal %v %v", refEntry(t, h, "demo"), err)
	}
}

func TestUnpinnedInstallAndForceReinstallKeepTrackingHead(t *testing.T) {
	h, ctx := refHome(t)
	repo, _, firstHead := c7PluginRepo(t, "name: demo\nversion: 1.0.0\n")
	target, _, _, err := installPluginCore(ctx, refURI(repo), installCoreOptions{})
	if err != nil || c6Git(t, target, "rev-parse", "HEAD") != firstHead || refEntry(t, h, "demo")["pinned"] != false {
		t.Fatalf("unpinned install %v %v", refEntry(t, h, "demo"), err)
	}
	nextHead := refCommit(t, repo, "later", "later")
	target, _, _, err = installPluginCore(ctx, refURI(repo), installCoreOptions{Force: true})
	entry := refEntry(t, h, "demo")
	if err != nil || c6Git(t, target, "rev-parse", "HEAD") != nextHead || entry["revision"] != nextHead || entry["pinned"] != false {
		t.Fatalf("force reinstall %v %v", entry, err)
	}
}

func TestFailedForceReinstallKeepsExistingPluginAndMetadata(t *testing.T) {
	h, ctx := refHome(t)
	repo, old, _ := c7PluginRepo(t, "name: demo\nversion: 1.0.0\n")
	target, _, _, err := installPluginCore(ctx, refURI(repo), installCoreOptions{Ref: old})
	if err != nil {
		t.Fatal(err)
	}
	before := refMetadata(t, h)
	_, _, _, err = installPluginCore(ctx, refURI(repo), installCoreOptions{Force: true, Ref: strings.Repeat("f", 40)})
	var opErr *PluginOperationError
	if !errors.As(err, &opErr) {
		t.Fatalf("force reinstall at a missing commit %v", err)
	}
	if !isDir(target) || c6Git(t, target, "rev-parse", "HEAD") != old || !reflect.DeepEqual(refMetadata(t, h), before) {
		t.Fatalf("existing install changed %v", refMetadata(t, h))
	}
}

func TestMetadataWriteFailureRollsBackNewInstall(t *testing.T) {
	h, ctx := refHome(t)
	repo, old, _ := c7PluginRepo(t, "name: demo\nversion: 1.0.0\n")
	metadata := filepath.Join(h, "plugins", ".install-metadata.json")
	t.Cleanup(pm.OverridePublicationWrite(func(path string, data []byte) error {
		if filepath.Clean(path) == filepath.Clean(metadata) {
			return errors.New("disk full")
		}
		return pm.DurableWriteBytes(path, data)
	}))
	_, _, _, err := installPluginCore(ctx, refURI(repo), installCoreOptions{Ref: old})
	var opErr *PluginOperationError
	if !errors.As(err, &opErr) || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("install with a failing metadata write %v", err)
	}
	if pathExists(filepath.Join(h, "plugins", "demo")) || pathExists(metadata) {
		t.Fatal("failed install left state behind")
	}
}

func TestMetadataWriteFailureRollsBackRemoval(t *testing.T) {
	h, ctx := refHome(t)
	repo, old, _ := c7PluginRepo(t, "name: demo\nversion: 1.0.0\n")
	target, _, _, err := installPluginCore(ctx, refURI(repo), installCoreOptions{Ref: old})
	if err != nil {
		t.Fatal(err)
	}
	before := refMetadata(t, h)
	previous := writeInstallMetadata
	writeInstallMetadata = func(context.Context, string, map[string]any) error { return errors.New("disk full") }
	t.Cleanup(func() { writeInstallMetadata = previous })
	if err := removePluginCore(ctx, h, target); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("remove with a failing metadata write %v", err)
	}
	if !isDir(target) || c6Git(t, target, "rev-parse", "HEAD") != old || !reflect.DeepEqual(refMetadata(t, h), before) {
		t.Fatal("removal was not rolled back")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(target), ".demo.remove-*")); len(left) != 0 {
		t.Fatalf("removal staging left %v", left)
	}
}

func TestAnnotatedTagPinInstallsAtItsCommit(t *testing.T) {
	h, ctx := refHome(t)
	repo, old, _ := c7PluginRepo(t, "name: demo\nversion: 1.0.0\n")
	c6Git(t, repo, "tag", "-a", "v1.0.2", old, "-m", "v1.0.2")
	tagObject := c6Git(t, repo, "rev-parse", "v1.0.2")
	if tagObject == old || c6Git(t, repo, "cat-file", "-t", tagObject) != "tag" {
		t.Fatalf("tag object %s", tagObject)
	}
	target, _, _, err := installPluginCore(ctx, refURI(repo), installCoreOptions{Ref: tagObject})
	if err != nil {
		t.Fatal(err)
	}
	if c6Git(t, target, "rev-parse", "HEAD") != old || gitPullRead(t, filepath.Join(target, "marker.txt")) != "old" ||
		refEntry(t, h, "demo")["revision"] != old {
		t.Fatalf("tag pin installed %s %v", c6Git(t, target, "rev-parse", "HEAD"), refEntry(t, h, "demo"))
	}
}

func TestInstallSavesAnHttpsUpdateUrlTag(t *testing.T) {
	h, ctx := refHome(t)
	repo, _, _ := c7PluginRepo(t, "name: demo\nversion: 1.0.0\n")
	c6Write(t, filepath.Join(repo, "plugin.yaml"), "name: demo\nupdate_url: ' https://feed.example/f.yml '\nversion: 1.0.0\n")
	refCommit(t, repo, "feed", "feed")
	if _, _, _, err := installPluginCore(ctx, refURI(repo), installCoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := refEntry(t, h, "demo")["update_url"]; got != "https://feed.example/f.yml" {
		t.Fatalf("update_url %v", got)
	}
}

// One commit plugin repository holding the given manifest
func manifestRepo(t *testing.T, manifest string) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	c6Write(t, filepath.Join(repo, "plugin.yaml"), manifest)
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "fixture@example.com"}, {"config", "user.name", "Fixture"}} {
		c6Git(t, repo, args...)
	}
	c6Commit(t, repo, "init")
	return repo
}

// Install with the install scan turned off, the way the manifest version cases patch it
func noScanInstall(t *testing.T) {
	t.Helper()
	previous := scanOnInstallEnabled
	scanOnInstallEnabled = func(string) bool { return false }
	t.Cleanup(func() { scanOnInstallEnabled = previous })
}

func TestInstallAcceptsEveryLoaderSupportedManifestVersion(t *testing.T) {
	for version := 1; version <= SupportedManifestVersion; version++ {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			_, ctx := refHome(t)
			noScanInstall(t)
			repo := manifestRepo(t, fmt.Sprintf("manifest_version: %d\nname: demo\nversion: 1.0.0\n", version))
			target, manifest, name, err := installPluginCore(ctx, refURI(repo), installCoreOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if name != "demo" || !pathExists(target) || fmt.Sprint(manifest["manifest_version"]) != fmt.Sprint(version) {
				t.Fatalf("install %s %v", name, manifest)
			}
		})
	}
}

func TestManifestVersionAboveSharedSupportIsRefusedCleanly(t *testing.T) {
	h, ctx := refHome(t)
	noScanInstall(t)
	repo := manifestRepo(t, fmt.Sprintf("manifest_version: %d\nname: demo\nversion: 1.0.0\n", SupportedManifestVersion+1))
	_, _, _, err := installPluginCore(ctx, refURI(repo), installCoreOptions{})
	var opErr *PluginOperationError
	if !errors.As(err, &opErr) || !regexp.MustCompile(fmt.Sprintf(`supports up to %d\b`, SupportedManifestVersion)).MatchString(err.Error()) {
		t.Fatalf("install above the supported version %v", err)
	}
	if pathExists(filepath.Join(h, "plugins", "demo")) || pathExists(filepath.Join(h, "plugins", ".install-metadata.json")) {
		t.Fatal("refused install left state behind")
	}
}

func TestALinkedPluginSlotIsAlreadyInstalledNotABadManifest(t *testing.T) {
	h, ctx := refHome(t)
	noScanInstall(t)
	repo := manifestRepo(t, "name: demo\nversion: 1.0.0\n")
	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(h, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(h, "plugins", "demo")); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := installPluginCore(ctx, refURI(repo), installCoreOptions{})
	var opErr *PluginOperationError
	if !errors.As(err, &opErr) || !strings.Contains(err.Error(), "already installed outside the catalog") || opErr.FailureClass != "already_installed" {
		t.Fatalf("install into a linked slot %v", err)
	}
}
