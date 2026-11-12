package plugins

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Points the in-tree kill list at a directory holding one removed.yaml row for the world repo
func catWorldRemoved(t *testing.T, w *catWorldT, reason string) {
	t.Helper()
	dir := t.TempDir()
	doc := "removed:\n- name: cat-plugin\n  repo: file://" + filepath.ToSlash(w.repo) + "\n  reason: " + reason + "\n"
	c6Write(t, filepath.Join(dir, "removed.yaml"), doc)
	t.Cleanup(SwapCatalogDir(dir))
}

func TestKillListBlocksCliDashboardAndTuiPaths(t *testing.T) {
	w := catWorld(t)
	catWorldRemoved(t, w, "malware")
	res, err := DashboardInstallPlugin(w.ctx, DashboardInstallOptions{CatalogName: "cat-plugin"})
	if err != nil || !strings.Contains(pyStrOr(res["error"]), "malware") {
		t.Fatalf("dashboard catalog install %v %v", res, err)
	}
	res, err = DashboardInstallPlugin(w.ctx, DashboardInstallOptions{Identifier: "file://" + filepath.ToSlash(w.repo)})
	if err != nil || !strings.Contains(pyStrOr(res["error"]), "malware") {
		t.Fatalf("dashboard url install %v %v", res, err)
	}
	target := filepath.Join(w.pluginsDir, "cat-plugin")
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("refused install left %s: %v", target, err)
	}
	disable := false
	var exit *CommandExit
	if err := CmdInstall(w.ctx, InstallCommandOptions{Identifier: "cat-plugin", Enable: &disable}, io.Discard); !errors.As(err, &exit) {
		t.Fatalf("cli install without --allow-removed = %v, want SystemExit", err)
	}
	if err := CmdInstall(w.ctx, InstallCommandOptions{Identifier: "cat-plugin", Enable: &disable, AllowRemoved: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if noErr(catalogInstallRecord(w.h, target)) == nil {
		t.Fatal("allow-removed install recorded no catalog provenance")
	}
	if reason, ok := noErr2(removedAnnotation(w.h, "cat-plugin", target, ResolvedRemovedEntries(w.ctx))); !ok || reason != "malware" {
		t.Fatalf("removed annotation %q %v", reason, ok)
	}
}

func TestUpdateOfASubdirInstallKeepsFilesTheUserCreatedOrEdited(t *testing.T) {
	for _, via := range []string{"url", "catalog"} {
		t.Run(via, func(t *testing.T) {
			w := catWorld(t)
			mono := filepath.Join(w.root, "mono")
			src := filepath.Join(mono, "plugins", "sub-plugin")
			c6Write(t, filepath.Join(src, "plugin.yaml"), "name: sub-plugin\nversion: 1.0.0\ndescription: d\n")
			c6Write(t, filepath.Join(src, "__init__.py"), "def register(ctx):\n    pass\n")
			c6Write(t, filepath.Join(src, "config.yaml.example"), "endpoint: default\n")
			c6Write(t, filepath.Join(src, "desktop", "plugin.js"), "export default { id: \"v1\" }\n")
			c6Write(t, filepath.Join(src, "server.js"), "console.log('v1')\n")
			c6Write(t, filepath.Join(src, "dashboard", "manifest.json"), "{}")
			c6Write(t, filepath.Join(src, "hooks", "run.cjs"), "module.exports = 1\n")
			c6Git(t, mono, "init", "-q")
			pin := c6Commit(t, mono, "v1")
			entry := func() *PluginCatalogEntry {
				return &PluginCatalogEntry{Name: "sub-plugin", Repo: "file://" + filepath.ToSlash(mono), Sha: pin,
					Description: "d", Maintainer: "t", Tier: "community", Subdir: "plugins/sub-plugin"}
			}
			inTreeCatalogEntries = func() []*PluginCatalogEntry { return []*PluginCatalogEntry{entry()} }
			var target string
			var err error
			if via == "catalog" {
				target, _, _, err = installCatalogEntry(w.ctx, entry(), CatalogInstallOptions{})
			} else {
				target, _, _, err = installPluginCore(w.ctx, "file://"+filepath.ToSlash(mono)+"#plugins/sub-plugin", installCoreOptions{})
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(target, ".git")); !os.IsNotExist(err) {
				t.Fatalf("subdir install carries .git: %v", err)
			}
			c6Write(t, filepath.Join(target, "config.yaml"), "endpoint: mine\n")
			c6Write(t, filepath.Join(target, "data", "state.json"), "{}")
			c6Write(t, filepath.Join(src, "plugin.yaml"), "name: sub-plugin\nversion: 2.0.0\ndescription: d\n")
			c6Write(t, filepath.Join(src, "config.yaml.example"), "endpoint: new-default\n")
			for _, gone := range []string{"desktop", "server.js", "dashboard", "hooks"} {
				if err := os.RemoveAll(filepath.Join(src, gone)); err != nil {
					t.Fatal(err)
				}
			}
			pin = c6Commit(t, mono, "v2")
			if res, err := DashboardUpdateUserPlugin(w.ctx, "sub-plugin", false, false); err != nil || res["ok"] != true {
				t.Fatalf("dashboard update %v %v", res, err)
			}
			if b, _ := os.ReadFile(filepath.Join(target, "plugin.yaml")); !strings.Contains(string(b), "version: 2.0.0") {
				t.Fatalf("plugin.yaml not updated: %s", b)
			}
			if b, _ := os.ReadFile(filepath.Join(target, "config.yaml")); string(b) != "endpoint: mine\n" {
				t.Fatalf("config.yaml = %q", b)
			}
			if b, _ := os.ReadFile(filepath.Join(target, "data", "state.json")); string(b) != "{}" {
				t.Fatalf("data/state.json = %q", b)
			}
			for _, gone := range []string{"desktop", "server.js", "dashboard", filepath.Join("hooks", "run.cjs")} {
				if _, err := os.Lstat(filepath.Join(target, gone)); !os.IsNotExist(err) {
					t.Fatalf("%s survived the update: %v", gone, err)
				}
			}
		})
	}
}

func TestRepinKeepsAWhollyIgnoredDataDirInAGitCheckout(t *testing.T) {
	w := catWorld(t)
	c6Write(t, filepath.Join(w.repo, ".gitignore"), "data/\n.venv/\nnode_modules/\n")
	w.pin = c6Commit(t, w.repo, "ignore data")
	target, _, _, err := installCatalogEntry(w.ctx, GetLiveCatalogEntry(w.ctx, "cat-plugin"), CatalogInstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, ".git")); err != nil {
		t.Fatalf("catalog install has no .git: %v", err)
	}
	c6Write(t, filepath.Join(target, "data", "db", "index.db"), "user data")
	if err := os.MkdirAll(filepath.Join(target, ".venv", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin/python3", filepath.Join(target, ".venv", "bin", "python")); err != nil {
		t.Fatal(err)
	}
	c6Write(t, filepath.Join(target, ".venv", "pyvenv.cfg"), "home = /usr/bin")
	c6Write(t, filepath.Join(target, "node_modules", "x", "index.js"), "module.exports = 1")
	c6Write(t, filepath.Join(w.repo, "__init__.py"), "def register(ctx):\n    pass  # v3\n")
	w.pin = c6Commit(t, w.repo, "v3")
	if res, err := DashboardUpdateUserPlugin(w.ctx, "cat-plugin", false, false); err != nil || res["unchanged"] != false {
		t.Fatalf("dashboard update %v %v", res, err)
	}
	if got := catHead(t, target); got != w.pin {
		t.Fatalf("head %s, want %s", got, w.pin)
	}
	if b, _ := os.ReadFile(filepath.Join(target, "data", "db", "index.db")); string(b) != "user data" {
		t.Fatalf("data/db/index.db = %q", b)
	}
	for _, gone := range []string{".venv", "node_modules"} {
		if _, err := os.Lstat(filepath.Join(target, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s carried into the update: %v", gone, err)
		}
	}
	published := w.pin
	outside := filepath.Join(filepath.Dir(w.repo), "outside.yaml")
	c6Write(t, outside, "secret")
	if err := os.Symlink(outside, filepath.Join(target, "data", "link.yaml")); err != nil {
		t.Fatal(err)
	}
	c6Write(t, filepath.Join(w.repo, "__init__.py"), "def register(ctx):\n    pass  # v4\n")
	w.pin = c6Commit(t, w.repo, "v4")
	res, err := DashboardUpdateUserPlugin(w.ctx, "cat-plugin", false, false)
	if err != nil || res["ok"] != false || !strings.Contains(pyStrOr(res["error"]), "data/link.yaml") {
		t.Fatalf("symlinked update %v %v", res, err)
	}
	if got := catHead(t, target); got != published {
		t.Fatalf("head %s, want %s", got, published)
	}
	if st, err := os.Lstat(filepath.Join(target, "data", "link.yaml")); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("data/link.yaml is no longer a symlink: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(target, "data", "db", "index.db")); string(b) != "user data" {
		t.Fatalf("data/db/index.db = %q", b)
	}
}

func TestRepinThatWidensThePluginRequiresConsentOnEverySurface(t *testing.T) {
	w := catWorld(t)
	target, _, _, err := installCatalogEntry(w.ctx, GetLiveCatalogEntry(w.ctx, "cat-plugin"), CatalogInstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c6Write(t, filepath.Join(w.repo, "plugin.yaml"), "name: cat-plugin\nversion: 3.0.0\ndescription: d\nprovides_tools: [shell_out]\n")
	c6Write(t, filepath.Join(w.repo, "desktop", "plugin.js"), "export default {}\n")
	wide := c6Commit(t, w.repo, "widen")
	w.pin = wide
	res, err := DashboardUpdateUserPlugin(w.ctx, "cat-plugin", false, false)
	if err != nil || res["ok"] != false || res["consent_required"] != true {
		t.Fatalf("widening update %v %v", res, err)
	}
	want := map[string][]string{"tools": {"shell_out"}, "desktop": {"desktop/plugin.js"}}
	if !reflect.DeepEqual(res["delta"], want) {
		t.Fatalf("delta %#v, want %#v", res["delta"], want)
	}
	if got := catHead(t, target); got != w.sha1 {
		t.Fatalf("head moved to %s", got)
	}
	savedTTY, savedInput := isTTY, inputLine
	t.Cleanup(func() { isTTY, inputLine = savedTTY, savedInput })
	isTTY = func() bool { return true }
	inputLine = func(string) (string, error) { return "n", nil }
	var exit *CommandExit
	if err := CmdUpdate(w.ctx, "cat-plugin", true, false, io.Discard); !errors.As(err, &exit) {
		t.Fatalf("declined cli update = %v, want SystemExit", err)
	}
	if got := catHead(t, target); got != w.sha1 {
		t.Fatalf("declined update moved head to %s", got)
	}
	if res, err := DashboardUpdateUserPlugin(w.ctx, "cat-plugin", true, false); err != nil || res["unchanged"] != false {
		t.Fatalf("consented update %v %v", res, err)
	}
	if got := catHead(t, target); got != wide {
		t.Fatalf("head %s, want %s", got, wide)
	}
}

// Value of a call whose Python form raises, the raise failing the case
func noErr[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// Pair of a call whose Python form raises, the raise failing the case
func noErr2[A, B any](a A, b B, err error) (A, B) {
	if err != nil {
		panic(err)
	}
	return a, b
}

func TestReadCatalogSidecarRaisesWhenTheLegacyMigrationCannotWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("this case needs a non root user, the read only plugins dir does not stop root")
	}
	sha := strings.Repeat("b", 40)
	h := c6Home(t, map[string]any{"legacy": map[string]any{"source": "https://github.com/o/r", "revision": sha,
		"pinned": true, "catalog_name": "legacy-entry", "catalog_tier": "verified"}})
	plugins := home.PluginsDir(h)
	target := filepath.Join(plugins, "legacy")
	if err := os.Chmod(plugins, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(plugins, 0o755) })
	if got, err := readCatalogSidecar(h, target); err == nil || got != nil {
		t.Fatalf("migration write failure answered %v %v, want the raise", got, err)
	}
	if _, err := catalogAnnotation(h, target); err == nil {
		t.Fatal("catalog annotation swallowed the migration failure")
	}
}
