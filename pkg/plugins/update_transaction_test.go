package plugins

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Installed transactional plugin over a real PM publication, its origin repo and the catalog pin state
type txWorld struct {
	ctx                   context.Context
	root, h, repo, target string
	sha                   string
}

// Installs the oauth_surface service the top level wiring provides, empty for manifests without sign ins
func txOAuthSurface(t *testing.T) {
	t.Helper()
	previous := services.Load()
	next := &Services{}
	if previous != nil {
		*next = *previous
	}
	next.OAuthSurface = func(manifest map[string]any) map[string]bool { return map[string]bool{} }
	InstallServices(next)
	t.Cleanup(func() { InstallServices(previous) })
}

// Commits a release of the transactional plugin with its path dependency libraries
func txVersion(t *testing.T, repo, version string, broken bool, minimum string, libraries ...string) string {
	t.Helper()
	if len(libraries) == 0 {
		libraries = []string{"shared-library"}
	}
	c6Write(t, filepath.Join(repo, "plugin.yaml"), fmt.Sprintf("name: transactional\nversion: %s\nmanifest_version: %d\nrequires_hermes: '%s'\n",
		version, SupportedManifestVersion, minimum))
	c6Write(t, filepath.Join(repo, "helper.py"), fmt.Sprintf("VERSION = %s\ndef register(ctx):\n    pass\n", pyrepr.Repr(version)))
	c6Write(t, filepath.Join(repo, ".gitignore"), "node_modules/\n")
	deps := `["impossible-plugin-dep==1", "impossible-plugin-dep==2"]`
	sources := ""
	quoted := []string{}
	for _, name := range libraries {
		c6Write(t, filepath.Join(filepath.Dir(repo), name, "pyproject.toml"), fmt.Sprintf("[project]\nname=\"%s\"\nversion=\"1\"\nrequires-python=\">=3.14\"\n[tool.uv]\npackage=false\n", name))
		sources += fmt.Sprintf("%s={path=\"../%s\"}\n", name, name)
		quoted = append(quoted, `"`+name+`"`)
	}
	if !broken {
		deps = "[" + strings.Join(quoted, ", ") + "]"
	}
	c6Write(t, filepath.Join(repo, "pyproject.toml"), fmt.Sprintf("[project]\nname=\"transactional\"\nversion=\"%s\"\nrequires-python=\">=3.14\"\n"+
		"dependencies=%s\n[tool.uv]\npackage=false\n[tool.uv.sources]\n%s", version, deps, sources))
	return c6Commit(t, repo, version)
}

// Copies a directory tree the way shutil.copytree does with dirs_exist_ok
func txCopyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), raw, info.Mode())
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Transactional plugin installed from the catalog or a custom git source, enabled, offline catalog seams in place
func txInstalled(t *testing.T, catalog bool) *txWorld {
	t.Helper()
	c6RequireGit(t)
	root, h := pubWorld(t)
	w := &txWorld{ctx: home.WithOverride(context.Background(), h), root: root, h: h, repo: filepath.Join(root, "plugin-origin")}
	if err := os.MkdirAll(w.repo, 0o755); err != nil {
		t.Fatal(err)
	}
	c6Git(t, w.repo, "init", "-qb", "main")
	w.sha = txVersion(t, w.repo, "1.0.0", false, "")
	entry := func() *PluginCatalogEntry {
		return &PluginCatalogEntry{Name: "transactional", Repo: "file://" + filepath.ToSlash(w.repo), Sha: w.sha, Description: "fixture",
			Maintainer: "fixture", Tier: "community", Category: "desktop"}
	}
	savedURL, savedClient, savedTree, savedScan := LiveCatalogURL, liveCatalogClient, inTreeCatalogEntries, scanOnInstallEnabled
	t.Cleanup(func() {
		LiveCatalogURL, liveCatalogClient, inTreeCatalogEntries, scanOnInstallEnabled = savedURL, savedClient, savedTree, savedScan
	})
	LiveCatalogURL = "http://127.0.0.1:9/nope"
	liveCatalogClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })}
	inTreeCatalogEntries = func() []*PluginCatalogEntry { return []*PluginCatalogEntry{entry()} }
	scanOnInstallEnabled = func(string) bool { return false }
	txOAuthSurface(t)
	var err error
	if catalog {
		w.target, _, _, err = installCatalogEntry(w.ctx, entry(), CatalogInstallOptions{})
	} else {
		w.target, _, _, err = installPluginCore(w.ctx, "file://"+filepath.ToSlash(w.repo), installCoreOptions{})
	}
	if err != nil {
		t.Fatal(err)
	}
	txCopyTree(t, filepath.Join(root, "shared-library"), filepath.Join(filepath.Dir(w.target), "shared-library"))
	if err := setPluginEnabled(w.ctx, h, "transactional", true, nil, NewConsole(io.Discard)); err != nil {
		t.Fatal(err)
	}
	return w
}

func txRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (w *txWorld) selected(t *testing.T) string {
	t.Helper()
	venv, err := pm.SelectedVenv(w.ctx, filepath.Join(w.root, "core"))
	if err != nil {
		t.Fatal(err)
	}
	return venv
}

func (w *txWorld) head(t *testing.T) string { return c6Git(t, w.target, "rev-parse", "HEAD") }

// Runs a test body once per install kind
func txKinds(t *testing.T, kinds []string, body func(t *testing.T, w *txWorld)) {
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) { body(t, txInstalled(t, kind == "catalog")) })
	}
}

var bothKinds = []string{"catalog", "custom"}

func TestFailedUpdateKeepsCodeMetadataConfigAndEnvironment(t *testing.T) {
	for _, failure := range []string{"dependencies", "version", "publication", "manifest"} {
		t.Run(failure, func(t *testing.T) { txFailure(t, failure) })
	}
}

// One failure kind of an update that must leave every watched file, the checkout and the venv alone
func txFailure(t *testing.T, failure string) {
	{
		txKinds(t, bothKinds, func(t *testing.T, w *txWorld) {
			selected := w.selected(t)
			facts := pm.RuntimeFactsPath(w.ctx)
			watched := []string{filepath.Join(w.target, "plugin.yaml"), filepath.Join(w.target, "helper.py"), filepath.Join(w.target, "pyproject.toml"),
				filepath.Join(w.h, "config.yaml"), filepath.Join(w.h, "plugins", ".install-metadata.json"), facts}
			before := map[string][]byte{}
			for _, path := range watched {
				before[path] = txRead(t, path)
			}
			oldHead := w.head(t)
			minimum := ""
			if failure == "version" {
				minimum = ">=999.0.0"
				saved := RunningVersion
				t.Cleanup(func() { RunningVersion = saved })
				RunningVersion = "1.0.0"
			}
			w.sha = txVersion(t, w.repo, "2.0.0", failure == "dependencies", minimum)
			if failure == "manifest" {
				c6Write(t, filepath.Join(w.repo, "plugin.yaml"), "name: [broken")
				w.sha = c6Commit(t, w.repo, "invalid manifest")
			}
			if failure == "publication" {
				dir := filepath.Dir(facts)
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(dir, 0o755) })
			}
			result, err := DashboardUpdateUserPlugin(w.ctx, "transactional", false, false)
			if failure == "publication" {
				os.Chmod(filepath.Dir(facts), 0o755)
			}
			if err != nil || result["ok"] != false || !pyrepr.Truthy(result["error"]) {
				t.Fatalf("%s: %v %v", failure, result, err)
			}
			if w.selected(t) != selected || w.head(t) != oldHead {
				t.Fatalf("%s moved the environment or the checkout", failure)
			}
			for _, path := range watched {
				if !bytes.Equal(txRead(t, path), before[path]) {
					t.Fatalf("%s changed %s", failure, path)
				}
			}
		})
	}
}

func TestSuccessfulUpdatePublishesMatchingCodeAndDurableWorkspace(t *testing.T) {
	txKinds(t, bothKinds, func(t *testing.T, w *txWorld) {
		before := w.selected(t)
		config := txRead(t, filepath.Join(w.h, "config.yaml"))
		w.sha = txVersion(t, w.repo, "2.0.0", false, "")
		result, err := DashboardUpdateUserPlugin(w.ctx, "transactional", false, false)
		if err != nil || result["ok"] != true {
			t.Fatalf("%v %v", result, err)
		}
		if w.head(t) != w.sha || !strings.Contains(string(txRead(t, filepath.Join(w.target, "helper.py"))), "VERSION = '2.0.0'") ||
			!bytes.Equal(txRead(t, filepath.Join(w.h, "config.yaml")), config) {
			t.Fatal("published code or config does not match")
		}
		venvFact := func() map[string]any {
			facts, err := pm.NewFacts(pm.RuntimeFactsPath(w.ctx), false)
			if err != nil {
				t.Fatal(err)
			}
			fact, _ := facts.Get("venv")
			return fact
		}
		fact := venvFact()
		extras := []string{}
		for _, e := range toList(fact["extras"]) {
			extras = append(extras, pyrepr.Str(e))
		}
		stamp, err := pm.NewVenv(filepath.Join(w.root, "core")).(*pm.Venv).ExpectedStamp(extras)
		if err != nil || w.selected(t) == before || fact["stamp"] != stamp {
			t.Fatalf("%v %v %v", err, fact["stamp"], stamp)
		}
		workspace := filepath.Dir(pyrepr.Str(fact["resolved_lock"]))
		var document map[string]any
		if err := toml.Unmarshal(txRead(t, filepath.Join(workspace, "pyproject.toml")), &document); err != nil {
			t.Fatal(err)
		}
		tool, _ := document["tool"].(map[string]any)
		uv, _ := tool["uv"].(map[string]any)
		ws, _ := uv["workspace"].(map[string]any)
		members := toList(ws["members"])
		if len(members) == 0 {
			t.Fatal("workspace has no members")
		}
		for _, member := range members {
			path := resolvePath(filepath.Join(workspace, pyrepr.Str(member)))
			if rel, err := filepath.Rel(workspace, path); err != nil || strings.HasPrefix(rel, "..") {
				t.Fatalf("member %s escapes the workspace", path)
			}
			if _, err := os.Stat(filepath.Join(path, "pyproject.toml")); err != nil {
				t.Fatal(err)
			}
		}
		metadata, _ := readInstallMetadata(w.h)
		record, _ := metadata["transactional"].(map[string]any)
		if record["revision"] != w.sha {
			t.Fatalf("record %v", record)
		}
		previousStamp := fact["stamp"]
		c6Write(t, filepath.Join(w.repo, "helper.py"), "VERSION = 'code-only'\ndef register(ctx):\n    pass\n")
		w.sha = c6Commit(t, w.repo, "code-only update")
		if result, err := DashboardUpdateUserPlugin(w.ctx, "transactional", false, false); err != nil || result["ok"] != true {
			t.Fatalf("%v %v", result, err)
		}
		metadata, _ = readInstallMetadata(w.h)
		record, _ = metadata["transactional"].(map[string]any)
		if venvFact()["stamp"] == previousStamp {
			t.Fatal("a code only update kept the venv stamp")
		}
		if block, _ := record["catalog"].(map[string]any); pyrepr.Truthy(block["name"]) {
			w.sha = txVersion(t, w.repo, "3.0.0", false, "")
			noNetwork := func(context.Context, string) (string, error) {
				t.Error("catalog pin must not consult a custom update source")
				return "", errors.New("no network")
			}
			var check *CheckResult
			for _, row := range RunChecks(w.ctx, filepath.Join(w.h, "plugins"), RunChecksOptions{Fetch: noNetwork, LsRemote: noNetwork}) {
				if row.Name == "transactional" {
					check = row
				}
			}
			if check == nil || check.Class != "catalog" || check.UpdateAvailable == nil || !*check.UpdateAvailable ||
				check.Current != record["revision"] || check.Latest != w.sha {
				t.Fatalf("%+v", check)
			}
		}
	})
}

func TestCLIUpdateScanRefusalKeepsTheInstalledTree(t *testing.T) {
	txKinds(t, bothKinds, func(t *testing.T, w *txWorld) {
		old := txRead(t, filepath.Join(w.target, "helper.py"))
		config := txRead(t, filepath.Join(w.h, "config.yaml"))
		w.sha = txVersion(t, w.repo, "2.0.0", false, "")
		var scans []string
		saved := scanPluginTree
		t.Cleanup(func() { scanPluginTree = saved })
		scanPluginTree = func(_, staged, _ string, _ scanOptions) (*skills.ScanResult, error) {
			scans = append(scans, staged)
			if staged == w.target || !bytes.Equal(txRead(t, filepath.Join(w.target, "helper.py")), old) {
				t.Error("scan ran against the live tree")
			}
			return nil, newPluginScanBlocked("fixture refuses candidate", nil)
		}
		var exit *CommandExit
		if err := CmdUpdate(w.ctx, "transactional", false, false, io.Discard); !errors.As(err, &exit) {
			t.Fatalf("%v", err)
		}
		if len(scans) != 1 || !bytes.Equal(txRead(t, filepath.Join(w.target, "helper.py")), old) || !bytes.Equal(txRead(t, filepath.Join(w.h, "config.yaml")), config) {
			t.Fatalf("scans %v", scans)
		}
	})
}

func TestDisabledUpdateDoesNotChangeDependenciesOrEnablement(t *testing.T) {
	txKinds(t, bothKinds, func(t *testing.T, w *txWorld) {
		if err := setPluginEnabled(w.ctx, w.h, "transactional", false, nil, NewConsole(io.Discard)); err != nil {
			t.Fatal(err)
		}
		config, facts := txRead(t, filepath.Join(w.h, "config.yaml")), txRead(t, pm.RuntimeFactsPath(w.ctx))
		w.sha = txVersion(t, w.repo, "2.0.0", false, "")
		result, err := DashboardUpdateUserPlugin(w.ctx, "transactional", false, false)
		if err != nil || result["ok"] != true || w.head(t) != w.sha {
			t.Fatalf("%v %v", result, err)
		}
		if !bytes.Equal(txRead(t, filepath.Join(w.h, "config.yaml")), config) || !bytes.Equal(txRead(t, pm.RuntimeFactsPath(w.ctx)), facts) {
			t.Fatal("disabled plugin changed the dependency selection or enablement")
		}
	})
}

func TestEnablementBeforeLockAcquisitionCannotSkipValidation(t *testing.T) {
	txKinds(t, bothKinds, func(t *testing.T, w *txWorld) {
		if err := setPluginEnabled(w.ctx, w.h, "transactional", false, nil, NewConsole(io.Discard)); err != nil {
			t.Fatal(err)
		}
		watched := []string{filepath.Join(w.target, "helper.py"), filepath.Join(w.target, "pyproject.toml"), filepath.Join(w.h, "plugins", ".install-metadata.json")}
		before := map[string][]byte{}
		for _, path := range watched {
			before[path] = txRead(t, path)
		}
		w.sha = txVersion(t, w.repo, "2.0.0", true, "")
		var enabledConfig, enabledFacts []byte
		saved := syncVenv
		t.Cleanup(func() { syncVenv = saved })
		syncVenv = func(ctx context.Context, opts pm.SyncVenvOptions) error {
			if enabledConfig == nil {
				if err := setPluginEnabled(w.ctx, w.h, "transactional", true, nil, NewConsole(io.Discard)); err != nil {
					t.Fatal(err)
				}
				enabledConfig, enabledFacts = txRead(t, filepath.Join(w.h, "config.yaml")), txRead(t, pm.RuntimeFactsPath(w.ctx))
			}
			return saved(ctx, opts)
		}
		result, err := DashboardUpdateUserPlugin(w.ctx, "transactional", false, false)
		if err != nil || result["ok"] != false {
			t.Fatalf("%v %v", result, err)
		}
		for _, path := range watched {
			if !bytes.Equal(txRead(t, path), before[path]) {
				t.Fatalf("changed %s", path)
			}
		}
		if enabledConfig == nil || !bytes.Equal(txRead(t, filepath.Join(w.h, "config.yaml")), enabledConfig) || !bytes.Equal(txRead(t, pm.RuntimeFactsPath(w.ctx)), enabledFacts) {
			t.Fatal("the concurrent enablement was lost or validation was skipped")
		}
	})
}

// Demo plugin repo with two commits and an install pinned to the first one
func txPinnedDemo(t *testing.T) (context.Context, string, string) {
	t.Helper()
	c6RequireGit(t)
	_, h := pubWorld(t)
	ctx := home.WithOverride(context.Background(), h)
	repo := filepath.Join(t.TempDir(), "demo-repo")
	c6Write(t, filepath.Join(repo, "plugin.yaml"), "name: demo\nversion: 1.0.0\n")
	c6Write(t, filepath.Join(repo, "helper.py"), "def register(ctx):\n    pass\n")
	c6Git(t, repo, "init", "-q")
	oldSha := c6Commit(t, repo, "v1")
	c6Write(t, filepath.Join(repo, "plugin.yaml"), "name: demo\nversion: 2.0.0\n")
	c6Commit(t, repo, "v2")
	if _, _, _, err := installPluginCore(ctx, "file://"+filepath.ToSlash(repo), installCoreOptions{Ref: oldSha}); err != nil {
		t.Fatal(err)
	}
	return ctx, h, oldSha
}

func TestPinnedPluginCmdUpdateRefusesToDrift(t *testing.T) {
	ctx, h, oldSha := txPinnedDemo(t)
	var out bytes.Buffer
	var exit *CommandExit
	if err := CmdUpdate(ctx, "demo", false, false, &out); !errors.As(err, &exit) || exit.Code != 1 {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(strings.ToLower(out.String()), "pinned") || c6Git(t, filepath.Join(h, "plugins", "demo"), "rev-parse", "HEAD") != oldSha {
		t.Fatal(out.String())
	}
}

func TestDashboardUpdateAlsoRefusesToDriftPin(t *testing.T) {
	ctx, h, oldSha := txPinnedDemo(t)
	result, err := DashboardUpdateUserPlugin(ctx, "demo", false, false)
	if err != nil || result["ok"] != false || !strings.Contains(pyrepr.Str(result["error"]), "pinned") {
		t.Fatalf("%v %v", result, err)
	}
	if c6Git(t, filepath.Join(h, "plugins", "demo"), "rev-parse", "HEAD") != oldSha {
		t.Fatal("pinned install drifted")
	}
}

// Commits a release the real scanner rates caution
func txCautionRelease(t *testing.T, repo string) string {
	t.Helper()
	txVersion(t, repo, "2.0.0", false, "")
	c6Write(t, filepath.Join(repo, "helper.py"), "eval('1 + 1')\n")
	return c6Commit(t, repo, "caution release")
}

func TestDashboardUpdateReturnsTheFindingsThenForceApplies(t *testing.T) {
	txKinds(t, []string{"custom"}, func(t *testing.T, w *txWorld) {
		oldHead, newHead := w.head(t), txCautionRelease(t, w.repo)
		scanOnInstallEnabled = func(string) bool { return true }
		blocked, err := DashboardUpdateUserPlugin(w.ctx, "transactional", false, false)
		if err != nil || blocked["ok"] != false || blocked["scan_blocked"] != true || blocked["scan_verdict"] != "caution" {
			t.Fatalf("%v %v", blocked, err)
		}
		found := false
		for _, f := range toList(blocked["scan_findings"]) {
			if m, ok := f.(map[string]any); ok && m["file"] == "helper.py" {
				found = true
			}
		}
		if !found || w.head(t) != oldHead {
			t.Fatalf("a blocked update changed the installed tree or lost its findings: %v", blocked["scan_findings"])
		}
		applied, err := DashboardUpdateUserPlugin(w.ctx, "transactional", false, true)
		if err != nil || applied["ok"] != true || applied["unchanged"] != false || w.head(t) != newHead {
			t.Fatalf("%v %v", applied, err)
		}
	})
}

func TestCLIUpdateAsksOnCautionAndForceSkipsTheQuestion(t *testing.T) {
	for _, tc := range []struct {
		name, answer     string
		force, published bool
	}{{"yes", "y", false, true}, {"no", "n", false, false}, {"force", "", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			txKinds(t, []string{"custom"}, func(t *testing.T, w *txWorld) {
				oldHead, newHead := w.head(t), txCautionRelease(t, w.repo)
				scanOnInstallEnabled = func(string) bool { return true }
				savedTTY, savedInput := isTTY, inputLine
				t.Cleanup(func() { isTTY, inputLine = savedTTY, savedInput })
				var asked []string
				isTTY = func() bool { return true }
				inputLine = func(prompt string) (string, error) { asked = append(asked, prompt); return tc.answer, nil }
				err := CmdUpdate(w.ctx, "transactional", isTTY(), tc.force, io.Discard)
				var exit *CommandExit
				if tc.published && err != nil || !tc.published && !errors.As(err, &exit) {
					t.Fatalf("%v", err)
				}
				prompts := 0
				for _, p := range asked {
					if strings.Contains(p, "Update anyway?") {
						prompts++
					}
				}
				want := oldHead
				if tc.published {
					want = newHead
				}
				if (prompts > 0) == tc.force || w.head(t) != want {
					t.Fatalf("prompts %v head %s", asked, w.head(t))
				}
			})
		})
	}
}
