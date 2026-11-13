package plugins

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

type catWorldT struct {
	ctx           context.Context
	root, h, repo string
	sha1, sha2    string
	pin, requires string
	pluginsDir    string
	name, tier    string
	category      string
}

// File repo with two commits, a catalog entry pinned to the first, offline catalog seams and a real core
func catWorld(t *testing.T) *catWorldT {
	t.Helper()
	return catWorldNamed(t, "cat-plugin", "def register(ctx):\n    pass\n", "def register(ctx):\n    pass  # v2\n")
}

// Catalog world of one named plugin whose two commits carry the given helper.py bodies, at tier community
func catWorldNamed(t *testing.T, name, v1, v2 string) *catWorldT {
	t.Helper()
	c6RequireGit(t)
	root, h := pubWorld(t)
	w := &catWorldT{ctx: home.WithOverride(context.Background(), h), root: root, h: h, repo: filepath.Join(root, "repo"),
		pluginsDir: filepath.Join(h, "plugins"), name: name, tier: "community", category: "tools"}
	c6Write(t, filepath.Join(w.repo, "plugin.yaml"), "name: "+name+"\nversion: 1.0.0\ndescription: d\n")
	c6Write(t, filepath.Join(w.repo, "helper.py"), v1)
	c6Git(t, w.repo, "init", "-q")
	w.sha1 = c6Commit(t, w.repo, "v1")
	c6Write(t, filepath.Join(w.repo, "helper.py"), v2)
	w.sha2 = c6Commit(t, w.repo, "v2")
	w.pin = w.sha1
	savedURL, savedClient, savedTree, savedScan, savedVersion := LiveCatalogURL, liveCatalogClient, inTreeCatalogEntries, scanOnInstallEnabled, RunningVersion
	t.Cleanup(func() {
		LiveCatalogURL, liveCatalogClient, inTreeCatalogEntries, scanOnInstallEnabled, RunningVersion = savedURL, savedClient, savedTree, savedScan, savedVersion
	})
	LiveCatalogURL = "http://127.0.0.1:9/nope"
	liveCatalogClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })}
	inTreeCatalogEntries = func() []*PluginCatalogEntry {
		return []*PluginCatalogEntry{{Name: w.name, Repo: "file://" + filepath.ToSlash(w.repo), Sha: w.pin, Description: "d",
			Maintainer: "t", Tier: w.tier, Category: w.category, RequiresIcebath: w.requires}}
	}
	scanOnInstallEnabled = func(string) bool { return false }
	txOAuthSurface(t)
	next := *services.Load()
	next.DefinesMemoryProvider = func(string) bool { return false }
	next.RequiresAuthNotice = func(map[string]any) string { return "" }
	next.DeclaresModelProviderKind = func(string) bool { return false }
	next.PluginOAuthKey = func(string) string { return "" }
	next.ForgetPluginGrants = func(string) error { return nil }
	next.OAuthAdmissionProblems = func(map[string]any, string) []string { return nil }
	next.OAuthAdmissionWarnings = func(map[string]any, string) []string { return nil }
	next.GatewayRunningPid = func(string, bool) (int, bool, error) { return 0, false, nil }
	next.InstalledGatewayServiceKind = func(context.Context) (string, bool, error) { return "", false, nil }
	next.RefreshPluginSessions = func(context.Context, string, string, string) error { return nil }
	InstallServices(&next)
	return w
}

func catHead(t *testing.T, dir string) string { return c6Git(t, dir, "rev-parse", "HEAD") }

func TestUpdateKeepsTheInstalledPinWhenTheNewPinNeedsANewerIcebath(t *testing.T) {
	w := catWorld(t)
	RunningVersion = "0.30.0"
	const refusal = "cat-plugin needs Icebath >=0.31; this is 0.30.0."
	installed, err := DashboardInstallPlugin(w.ctx, DashboardInstallOptions{CatalogName: "cat-plugin"})
	if err != nil || installed["ok"] != true {
		t.Fatalf("install %v %v", installed, err)
	}
	target := filepath.Join(w.pluginsDir, "cat-plugin")
	w.pin, w.requires = w.sha2, ">=0.31"
	var out bytes.Buffer
	var exit *CommandExit
	if err := CmdUpdate(w.ctx, "cat-plugin", false, false, &out); !errors.As(err, &exit) {
		t.Fatalf("update %v", err)
	}
	found := false
	for _, line := range strings.Split(out.String(), "\n") {
		found = found || strings.Contains(line, refusal) && strings.Contains(line, "after you update Icebath")
	}
	if !found {
		t.Fatalf("printed %s", out.String())
	}
	dashboard, err := DashboardUpdateUserPlugin(w.ctx, "cat-plugin", false, false)
	if err != nil || dashboard["ok"] != false || !strings.Contains(dashboard["error"].(string), refusal) {
		t.Fatalf("dashboard %v %v", dashboard, err)
	}
	plan := PlanUpdateAll(RunChecks(w.ctx, w.pluginsDir, RunChecksOptions{}))
	if len(plan.Movable) != 0 || len(plan.Skipped) != 1 || plan.Skipped[0].Row.Name != "cat-plugin" ||
		!strings.Contains(plan.Skipped[0].Reason, "after updating Icebath") {
		t.Fatalf("plan %+v", plan)
	}
	if catHead(t, target) != w.sha1 {
		t.Fatal("the installed pin moved")
	}
	RunningVersion = "0.31.0"
	moved, err := DashboardUpdateUserPlugin(w.ctx, "cat-plugin", false, false)
	if err != nil || moved["unchanged"] != false || catHead(t, target) != w.sha2 {
		t.Fatalf("update after upgrade %v %v", moved, err)
	}
}

func TestTheCatalogNameYouInstalledWithWorksForEveryLaterVerb(t *testing.T) {
	w := catWorld(t)
	c6Write(t, filepath.Join(w.repo, "plugin.yaml"), "name: renamed-plugin\nversion: 1.0.0\ndescription: d\n")
	w.pin = c6Commit(t, w.repo, "manifest name differs from catalog name")
	evil := filepath.Join(w.root, "evil")
	c6Write(t, filepath.Join(evil, "plugin.yaml"), "name: evil-plugin\nversion: 1.0.0\ndescription: d\n")
	c6Write(t, filepath.Join(evil, "helper.py"), "def register(ctx):\n    pass\n")
	c6Write(t, filepath.Join(evil, CatalogSidecar), `{"catalog_name": "cat-plugin", "tier": "official", "sha": "`+strings.Repeat("0", 40)+`", "repo": "x"}`)
	c6Git(t, evil, "init", "-q")
	c6Commit(t, evil, "v1")
	if _, _, _, err := installPluginCore(w.ctx, "file://"+filepath.ToSlash(evil), installCoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if entry, err := findPluginEntry(w.ctx, "cat-plugin"); err != nil || entry != nil {
		t.Fatalf("forged sidecar claimed the catalog name %+v %v", entry, err)
	}
	disable := false
	if err := CmdInstall(w.ctx, InstallCommandOptions{Identifier: "cat-plugin", Enable: &disable}, io.Discard); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(w.pluginsDir, "renamed-plugin")
	if st, err := os.Stat(target); err != nil || !st.IsDir() || len(getEnabledSet(w.h)) != 0 {
		t.Fatalf("install target %v enabled %v", err, getEnabledSet(w.h))
	}
	if err := CmdEnable(w.ctx, "cat-plugin", nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := getEnabledSet(w.h); len(got) != 1 || !got["renamed-plugin"] {
		t.Fatalf("enabled %v", got)
	}
	if err := cmdShow(w.ctx, "cat-plugin", NewConsole(io.Discard)); err != nil {
		t.Fatal(err)
	}
	name := "cat-plugin"
	if err := CmdCapabilities(w.ctx, &name, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := CmdDisable(w.ctx, "cat-plugin", io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := getDisabledSet(w.h); len(got) != 1 || !got["renamed-plugin"] {
		t.Fatalf("disabled %v", got)
	}
	if res, err := DashboardSetAgentPluginEnabled(w.ctx, "cat-plugin", true); err != nil || res["name"] != "renamed-plugin" {
		t.Fatalf("dashboard enable %v %v", res, err)
	}
	if res, err := DashboardUpdateUserPlugin(w.ctx, "cat-plugin", false, false); err != nil || res["ok"] != true {
		t.Fatalf("dashboard update %v %v", res, err)
	}
	if res, err := DashboardRemoveUserPlugin(w.ctx, "cat-plugin"); err != nil || res["ok"] != true {
		t.Fatalf("dashboard remove %v %v", res, err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) || getEnabledSet(w.h)["renamed-plugin"] {
		t.Fatalf("remove left %v %v", err, getEnabledSet(w.h))
	}
}

// Self cloned plugin dir with an origin remote and no provenance row under a temp home
func adoptWorld(t *testing.T, origin string, git bool) (context.Context, string, string) {
	t.Helper()
	c6RequireGit(t)
	h := t.TempDir()
	t.Setenv(home.EnvHome, h)
	ctx := home.WithOverride(context.Background(), h)
	plugin := filepath.Join(h, "plugins", "plug")
	c6Write(t, filepath.Join(plugin, "plugin.yaml"), "name: plug\nversion: 1.0.0\n")
	c6Write(t, filepath.Join(plugin, "helper.py"), "def register(ctx):\n    pass\n")
	if git {
		c6Git(t, plugin, "init", "-q")
		if origin != "" {
			c6Git(t, plugin, "remote", "add", "origin", origin)
		}
		c6Commit(t, plugin, "v1")
	}
	return ctx, h, plugin
}

func TestAdoptRecordsTheOriginOfASelfClonedDir(t *testing.T) {
	ctx, h, plugin := adoptWorld(t, "https://github.com/o/r", true)
	var out bytes.Buffer
	if err := CmdAdopt(ctx, "plug", &out); err != nil {
		t.Fatalf("%v %s", err, out.String())
	}
	head := catHead(t, plugin)
	row, _ := ReadSidecarRows(filepath.Join(h, "plugins"))["plug"].(map[string]any)
	if row == nil || row["pinned"] != false || row["revision"] != head || row["source"] == "" {
		t.Fatalf("row %v", row)
	}
	if !strings.Contains(out.String(), "Adopted") || !strings.Contains(out.String(), "revision: "+head[:12]) {
		t.Fatalf("printed %s", out.String())
	}
	out.Reset()
	var exit *CommandExit
	if err := CmdAdopt(ctx, "plug", &out); !errors.As(err, &exit) || !strings.Contains(out.String(), "not self-cloned") {
		t.Fatalf("second adopt %v %s", err, out.String())
	}
}

func TestAdoptRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		origin string
		git    bool
		want   string
	}{
		"manual":     {"", false, "nothing to adopt"},
		"no origin":  {"", true, "no readable git origin remote"},
		"bad origin": {"justaname", true, "not installable"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, h, _ := adoptWorld(t, tc.origin, tc.git)
			var out bytes.Buffer
			var exit *CommandExit
			if err := CmdAdopt(ctx, "plug", &out); !errors.As(err, &exit) || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("%v %s", err, out.String())
			}
			if len(ReadSidecarRows(filepath.Join(h, "plugins"))) != 0 {
				t.Fatal("a refused adopt wrote a row")
			}
		})
	}
	ctx, _, _ := adoptWorld(t, "https://github.com/o/r", true)
	var out bytes.Buffer
	var exit *CommandExit
	if err := CmdAdopt(ctx, "absent", &out); !errors.As(err, &exit) {
		t.Fatalf("absent plugin %v %s", err, out.String())
	}
}

func TestTrustUpdateURLMovesTheSavedTagToTheManifest(t *testing.T) {
	ctx, h, plugin := adoptWorld(t, "https://github.com/o/r", true)
	plugins := filepath.Join(h, "plugins")
	var out bytes.Buffer
	var exit *CommandExit
	if err := CmdTrustUpdateURL(ctx, "plug", &out); !errors.As(err, &exit) || !strings.Contains(out.String(), "has no provenance row") {
		t.Fatalf("no row %v %s", err, out.String())
	}
	row := map[string]any{"pinned": false, "revision": catHead(t, plugin), "source": "https://github.com/o/r"}
	if err := writeInstallMetadata(ctx, h, map[string]any{"plug": row}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := CmdTrustUpdateURL(ctx, "plug", &out); err != nil || !strings.Contains(out.String(), "Nothing to trust") {
		t.Fatalf("no mismatch %v %s", err, out.String())
	}
	row["update_url"] = "https://a/f.yml"
	if err := writeInstallMetadata(ctx, h, map[string]any{"plug": row}); err != nil {
		t.Fatal(err)
	}
	c6Write(t, filepath.Join(plugin, "plugin.yaml"), "name: plug\nversion: 1.0.0\nupdate_url: http://b/f.yml\n")
	out.Reset()
	if err := CmdTrustUpdateURL(ctx, "plug", &out); !errors.As(err, &exit) || !strings.Contains(out.String(), "Not trusted.") {
		t.Fatalf("insecure %v %s", err, out.String())
	}
	if got := ReadSidecarRows(plugins)["plug"].(map[string]any)["update_url"]; got != "https://a/f.yml" {
		t.Fatalf("refused trust moved the tag to %v", got)
	}
	c6Write(t, filepath.Join(plugin, "plugin.yaml"), "name: plug\nversion: 1.0.0\nupdate_url: https://b/f.yml\n")
	out.Reset()
	if err := CmdTrustUpdateURL(ctx, "plug", &out); err != nil || !strings.Contains(out.String(), "old: https://a/f.yml\n  new: https://b/f.yml") {
		t.Fatalf("trust %v %s", err, out.String())
	}
	if got := ReadSidecarRows(plugins)["plug"].(map[string]any)["update_url"]; got != "https://b/f.yml" {
		t.Fatalf("tag %v", got)
	}
	c6Write(t, filepath.Join(plugin, "plugin.yaml"), "name: plug\nversion: 1.0.0\n")
	out.Reset()
	if err := CmdTrustUpdateURL(ctx, "plug", &out); err != nil || !strings.Contains(out.String(), "new: (none)") {
		t.Fatalf("dropped url %v %s", err, out.String())
	}
	if got, ok := ReadSidecarRows(plugins)["plug"].(map[string]any)["update_url"]; !ok || got != nil {
		t.Fatalf("dropped tag %v %v", got, ok)
	}
}
