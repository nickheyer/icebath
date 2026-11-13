package plugins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/skills"
)

func TestUrlInstallOfAnArchivedHandoffRepoIsRepinnedToItsCatalogEntry(t *testing.T) {
	w := catWorld(t)
	archived := filepath.Join(w.root, "archived")
	c6Write(t, filepath.Join(archived, "plugin.yaml"), "name: cat-plugin\nversion: 1.0.0\ndescription: d\n")
	c6Write(t, filepath.Join(archived, "helper.py"), "def register(ctx):\n    pass\n")
	c6Write(t, filepath.Join(archived, "ARCHIVED.md"), "moved\n")
	c6Git(t, archived, "init", "-q")
	c6Commit(t, archived, "v1")
	target, _, _, err := installPluginCore(w.ctx, "file://"+filepath.ToSlash(archived), installCoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sidecar := noErr(readCatalogSidecar(w.h, target)); sidecar != nil {
		t.Fatalf("an unknown repo stays a plain URL install %v", sidecar)
	}
	key := normalizeRepo("file://" + filepath.ToSlash(archived))
	handoffRepos[key] = "cat-plugin"
	t.Cleanup(func() { delete(handoffRepos, key) })
	if sidecar := noErr(readCatalogSidecar(w.h, target)); sidecar == nil || sidecar["catalog_name"] != "cat-plugin" {
		t.Fatalf("handoff sidecar %v", sidecar)
	}
	seen := map[string]bool{}
	realScan := scanPluginTree
	t.Cleanup(func() { scanPluginTree = realScan })
	scanPluginTree = func(h, pluginDir, identifier string, opts scanOptions) (*skills.ScanResult, error) {
		seen["reviewed_pin"] = opts.ReviewedPin
		return realScan(h, pluginDir, identifier, opts)
	}
	result, err := DashboardUpdateUserPlugin(w.ctx, "cat-plugin", false, false)
	if err != nil || result["ok"] != true || result["unchanged"] != false || catHead(t, target) != w.sha1 {
		t.Fatalf("update %v %v", result, err)
	}
	if !seen["reviewed_pin"] {
		t.Fatal("the re-pinned tree is the reviewed sha")
	}
	metadata, err := readInstallMetadata(w.h)
	if err != nil {
		t.Fatal(err)
	}
	record, _ := metadata["cat-plugin"].(map[string]any)
	catalog, _ := record["catalog"].(map[string]any)
	repoURL := "file://" + filepath.ToSlash(w.repo)
	if record["source"] != repoURL || catalog["repo"] != repoURL {
		t.Fatalf("record %v", record)
	}
}

func TestRepinKeepsLocalFilesBacksUpEditsAndFollowsManifestRename(t *testing.T) {
	w := catWorld(t)
	target, _, _, err := installCatalogEntry(w.ctx, GetLiveCatalogEntry(w.ctx, "cat-plugin"), CatalogInstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c6Write(t, filepath.Join(target, "config.yaml"), "api_key: real\n")
	c6Write(t, filepath.Join(target, "helper.py"), "def register(ctx):\n    pass  # mine\n")
	if err := writeConfigValue(w.ctx, w.h, "plugins", "enabled", []any{"cat-plugin"}); err != nil {
		t.Fatal(err)
	}
	c6Write(t, filepath.Join(w.repo, "plugin.yaml"), "name: cat-plugin-v2\nversion: 2.0.0\ndescription: d\n")
	w.pin = c6Commit(t, w.repo, "rename")
	result, err := DashboardUpdateUserPlugin(w.ctx, "cat-plugin", false, false)
	newTarget := filepath.Join(w.pluginsDir, "cat-plugin-v2")
	if err != nil || result["ok"] != true || result["name"] != "cat-plugin-v2" || catHead(t, newTarget) != w.pin {
		t.Fatalf("update %v %v", result, err)
	}
	if cfg := gitPullRead(t, filepath.Join(newTarget, "config.yaml")); cfg != "api_key: real\n" {
		t.Fatalf("config %q", cfg)
	}
	metadata, err := readInstallMetadata(w.h)
	if _, statErr := os.Stat(target); err != nil || !os.IsNotExist(statErr) || metadata["cat-plugin"] != nil {
		t.Fatalf("old install left %v %v %v", statErr, metadata, err)
	}
	if enabled := getEnabledSet(w.h); len(enabled) != 1 || !enabled["cat-plugin-v2"] {
		t.Fatalf("enabled %v", enabled)
	}
	backups, _ := filepath.Glob(filepath.Join(filepath.Dir(w.pluginsDir), "plugins-backup", "cat-plugin-*", "helper.py"))
	if len(backups) == 0 || !strings.Contains(gitPullRead(t, backups[0]), "# mine") {
		t.Fatalf("backups %v", backups)
	}
	warnings, _ := result["warnings"].([]string)
	backedUp, renamed := false, false
	for _, text := range warnings {
		backedUp = backedUp || strings.Contains(text, "plugins-backup")
		renamed = renamed || strings.Contains(text, "renamed")
	}
	if !backedUp || !renamed {
		t.Fatalf("warnings %v", result["warnings"])
	}
}
