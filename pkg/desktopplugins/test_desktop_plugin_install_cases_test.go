package desktopplugins

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Throwaway home and the local repository its https identifier is served from
type installCase struct {
	t    *testing.T
	tmp  string
	home string
	repo string
	url  string
}

// Resolved temp folder holding a fresh ICEBATH_HOME
func installHome(t *testing.T) *installCase {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := filepath.Join(tmp, "home")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	return &installCase{t: t, tmp: tmp, home: h}
}

// App level desktop-plugins folder of the case home
func (c *installCase) root() string { return filepath.Join(c.home, "desktop-plugins") }

// Runs git in dir with the user and system config left out and returns its trimmed output
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// Local repository served over loopback https by git http-backend, trusted through GIT_SSL_CAINFO
func (c *installCase) serveRepo() {
	t := c.t
	t.Helper()
	projects := filepath.Join(c.tmp, "served")
	c.repo = filepath.Join(projects, "acme", "gizmo.git")
	if err := os.MkdirAll(c.repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, c.repo, "init", "-q", "-b", "main")
	gitExe, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(&cgi.Handler{Path: gitExe, Args: []string{"http-backend"}, Env: []string{
		"GIT_PROJECT_ROOT=" + projects, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1"}})
	t.Cleanup(srv.Close)
	ca := filepath.Join(c.tmp, "loopback-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", ca)
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(k, "")
	}
	t.Setenv("NO_PROXY", "*")
	c.url = srv.URL + "/acme/gizmo.git"
}

// Writes the files, commits them and returns the new HEAD
func (c *installCase) commit(files map[string]string) string {
	t := c.t
	t.Helper()
	for rel, text := range files {
		writeFile(t, filepath.Join(c.repo, filepath.FromSlash(rel)), text)
	}
	gitIn(t, c.repo, "add", "-A")
	gitIn(t, c.repo, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-q", "-m", "c")
	return gitIn(t, c.repo, "rev-parse", "HEAD")
}

// Live catalog cache publishing gizmo from the served repository at sha
func (c *installCase) catalog(sha string) {
	t := c.t
	t.Helper()
	doc, err := json.Marshal(map[string]any{"entries": []any{map[string]any{"name": "gizmo", "repo": c.url, "sha": sha}}})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home.CacheDir(c.home), "plugin-catalog.json"), string(doc))
}

func (c *installCase) install(identifier, ref, catalogName string, force bool) map[string]any {
	c.t.Helper()
	result, err := InstallDesktopPlugin(context.Background(), identifier, ref, catalogName, force)
	if err != nil {
		c.t.Fatal(err)
	}
	return result
}

func (c *installCase) remove(name string, force bool) map[string]any {
	c.t.Helper()
	result, err := RemoveDesktopPlugin(name, force)
	if err != nil {
		c.t.Fatal(err)
	}
	return result
}

func okResult(t *testing.T, result map[string]any) {
	t.Helper()
	if result["ok"] != true {
		t.Fatalf("not ok: %v", result)
	}
}

func refusedWith(t *testing.T, result map[string]any, fragments ...string) {
	t.Helper()
	text, _ := result["error"].(string)
	if result["ok"] != false {
		t.Fatalf("not refused: %v", result)
	}
	for _, f := range fragments {
		if !strings.Contains(text, f) {
			t.Fatalf("error %q lacks %q", text, f)
		}
	}
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func isRegular(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

func TestDesktopOnlyRepoLandsUnderItsOwnNameWithoutAMarker(t *testing.T) {
	c := installHome(t)
	c.serveRepo()
	sha := c.commit(map[string]string{"plugin.js": "export default { id: 'gizmo' }\n", "panel.js": "export const x = 1\n",
		".hermes-package.json": `{"package": "victim", "source": "/x"}`})

	result := c.install(c.url, sha, "", false)

	okResult(t, result)
	if result["plugin_name"] != "gizmo" || result["paired_with_agent"] != false || result["sha"] != sha {
		t.Fatalf("result %v", result)
	}
	folder := filepath.Join(c.root(), "gizmo")
	if !strings.HasPrefix(readText(t, filepath.Join(folder, "plugin.js")), "export default") {
		t.Fatal("plugin.js not published")
	}
	if !isRegular(filepath.Join(folder, "panel.js")) {
		t.Fatal("panel.js not published")
	}
	if exists(filepath.Join(folder, ".hermes-package.json")) {
		t.Fatal("the repository's own marker was published")
	}
}

func TestPackageLandsItsDesktopHalfUnderTheAgentNameWithAMarker(t *testing.T) {
	c := installHome(t)
	c.serveRepo()
	sha := c.commit(map[string]string{"plugin.yaml": "name: pkg\nversion: 1.0.0\n", "__init__.py": "",
		"desktop/plugin.js": "export default { id: 'pkg' }\n"})

	result := c.install(c.url, sha, "", false)

	okResult(t, result)
	if result["plugin_name"] != "pkg" || result["paired_with_agent"] != true {
		t.Fatalf("result %v", result)
	}
	folder := filepath.Join(c.root(), "pkg")
	if !isRegular(filepath.Join(folder, "plugin.js")) {
		t.Fatal("plugin.js not published")
	}
	if exists(filepath.Join(folder, "plugin.yaml")) {
		t.Fatal("the agent payload was copied")
	}
	var marker map[string]any
	if err := json.Unmarshal([]byte(readText(t, filepath.Join(folder, ".hermes-package.json"))), &marker); err != nil {
		t.Fatal(err)
	}
	if marker["package"] != "pkg" || marker["repo"] != c.url || marker["source"] != folder {
		t.Fatalf("marker %v", marker)
	}
	if mtime, _ := marker["sourceMtimeMs"].(float64); mtime <= 0 {
		t.Fatalf("sourceMtimeMs %v", marker["sourceMtimeMs"])
	}
}

func TestTheInstallIsTheReviewedCommitAndNothingElse(t *testing.T) {
	c := installHome(t)
	c.serveRepo()
	reviewed := c.commit(map[string]string{"plugin.js": "export default { v: 1 }\n"})
	c.commit(map[string]string{"plugin.js": "export default { v: 2 }\n"})

	refusedWith(t, c.install(c.url, "", "", false))
	refusedWith(t, c.install(c.url, strings.Repeat("1", 40), "", false))
	if exists(filepath.Join(c.root(), "gizmo")) {
		t.Fatal("an unreviewed install was published")
	}

	result := c.install(c.url, reviewed, "", false)

	okResult(t, result)
	if !strings.Contains(readText(t, filepath.Join(c.root(), "gizmo", "plugin.js")), "v: 1") {
		t.Fatal("not the reviewed commit")
	}
}

func TestASecondInstallRefusesUntilForceReplacesIt(t *testing.T) {
	c := installHome(t)
	c.serveRepo()
	first := c.commit(map[string]string{"plugin.js": "export default { v: 1 }\n"})
	okResult(t, c.install(c.url, first, "", false))

	refusedWith(t, c.install(c.url, first, "", false), "already exists", "force")

	second := c.commit(map[string]string{"plugin.js": "export default { v: 2 }\n"})
	okResult(t, c.install(c.url, second, "", true))
	if !strings.Contains(readText(t, filepath.Join(c.root(), "gizmo", "plugin.js")), "v: 2") {
		t.Fatal("force did not replace the plugin")
	}
}

func TestALocalFolderSourceIsInstalledLikeTheCliInstallsIt(t *testing.T) {
	c := installHome(t)
	c.serveRepo()
	sha := c.commit(map[string]string{"plugin.js": "export default {}\n"})
	uri, err := pypath.AsURI(c.repo)
	if err != nil {
		t.Fatal(err)
	}

	result := c.install(uri, sha, "", false)

	okResult(t, result)
	name, _ := result["plugin_name"].(string)
	if !isRegular(filepath.Join(c.root(), name, "plugin.js")) {
		t.Fatalf("result %v", result)
	}
}

func TestALinkOutOfTheCloneIsRefused(t *testing.T) {
	c := installHome(t)
	c.serveRepo()
	outside := filepath.Join(c.tmp, "secrets")
	writeFile(t, filepath.Join(outside, "plugin.js"), "// any folder of the host\n")
	symlink(t, outside, filepath.Join(c.repo, "desktop"))
	sha := c.commit(map[string]string{"README.md": "x\n"})

	result := c.install(c.url, sha, "", false)

	refusedWith(t, result, "link")
	if exists(filepath.Join(c.root(), "gizmo")) {
		t.Fatal("a linked tree was published")
	}
}

func TestARepositoryWithNoDesktopHalfIsRefused(t *testing.T) {
	c := installHome(t)
	c.serveRepo()
	sha := c.commit(map[string]string{"plugin.yaml": "name: agently\n", "__init__.py": ""})

	result := c.install(c.url, sha, "", false)

	refusedWith(t, result, "no desktop plugin")
	if exists(filepath.Join(c.root(), "gizmo")) {
		t.Fatal("an agent only package was published")
	}
}

func TestANameThatIsAPathIsRefused(t *testing.T) {
	c := installHome(t)
	c.serveRepo()
	sha := c.commit(map[string]string{"plugin.yaml": "name: ../escape\n", "__init__.py": "",
		"desktop/plugin.js": "export default {}\n"})

	result := c.install(c.url, sha, "", false)

	refusedWith(t, result, "folder name")
	if exists(filepath.Join(c.home, "escape")) {
		t.Fatal("the name escaped the root")
	}
}

func TestRemoveTakesAStandalonePluginAway(t *testing.T) {
	c := installHome(t)
	c.serveRepo()
	sha := c.commit(map[string]string{"plugin.js": "export default {}\n"})
	okResult(t, c.install(c.url, sha, "", false))

	result := c.remove("gizmo", false)

	want := map[string]any{"ok": true, "plugin_name": "gizmo", "path": filepath.Join(c.root(), "gizmo"), "unpaired_from": nil}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("result %v", result)
	}
	if exists(filepath.Join(c.root(), "gizmo")) {
		t.Fatal("the plugin is still installed")
	}
}

func TestRemoveRefusesAPackagesOwnHalfUntilForced(t *testing.T) {
	c := installHome(t)
	c.serveRepo()
	sha := c.commit(map[string]string{"plugin.yaml": "name: pkg\n", "__init__.py": "", "desktop/plugin.js": "export default {}\n"})
	okResult(t, c.install(c.url, sha, "", false))

	refusedWith(t, c.remove("pkg", false), "uninstall that plugin instead")
	if !isRegular(filepath.Join(c.root(), "pkg", "plugin.js")) {
		t.Fatal("the refused removal deleted the half")
	}

	forced := c.remove("pkg", true)
	if forced["ok"] != true || forced["unpaired_from"] != "pkg" {
		t.Fatalf("forced %v", forced)
	}
	if exists(filepath.Join(c.root(), "pkg")) {
		t.Fatal("the forced removal left the half")
	}
}

func TestRemoveLeavesALinkedPluginAloneAndNamesNothingOutsideTheRoot(t *testing.T) {
	c := installHome(t)
	checkout := filepath.Join(c.tmp, "dev")
	writeFile(t, filepath.Join(checkout, "plugin.js"), "export default {}\n")
	if err := os.MkdirAll(c.root(), 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, checkout, filepath.Join(c.root(), "dev"))

	refusedWith(t, c.remove("dev", false), "linked")
	if !isRegular(filepath.Join(c.root(), "dev", "plugin.js")) {
		t.Fatal("the linked checkout was touched")
	}

	refusedWith(t, c.remove("../config.yaml", false), "folder name")
	refusedWith(t, c.remove("ghost", false), "No desktop plugin named 'ghost'")
}

func TestRemoveLeavesAFolderTheHostReportsAsAJunctionAlone(t *testing.T) {
	c := installHome(t)
	writeFile(t, filepath.Join(c.root(), "dev", "plugin.js"), "export default {}\n")
	stubJunctions(t, filepath.Join(c.root(), "dev"))

	result := c.remove("dev", true)

	refusedWith(t, result, "linked from somewhere else")
	if !isRegular(filepath.Join(c.root(), "dev", "plugin.js")) {
		t.Fatal("the junction was removed")
	}
}

func TestInstallDoesNotReplaceADanglingJunctionWithoutForce(t *testing.T) {
	c := installHome(t)
	c.serveRepo()
	sha := c.commit(map[string]string{"plugin.js": "export default {}\n"})
	stubJunctions(t, filepath.Join(c.root(), "gizmo"))

	result := c.install(c.url, sha, "", false)

	refusedWith(t, result, "already exists")
	if exists(filepath.Join(c.root(), "gizmo")) {
		t.Fatal("the junction was replaced")
	}
}

func TestRemoveRefusesANameWindowsReadsAsADriveOrADevice(t *testing.T) {
	for _, name := range []string{"C:", "C:..", "D:outside", "nul .js", "CON", "nul.txt", "Com1", "LPT¹.js", "CONIN$", "clock$.js", "gizmo."} {
		t.Run(name, func(t *testing.T) {
			c := installHome(t)
			if err := os.MkdirAll(filepath.Join(c.root(), "keep"), 0o755); err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" {
				if err := os.Mkdir(filepath.Join(c.root(), name), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			result := c.remove(name, true)

			refusedWith(t, result, "folder name")
			if !isDir(filepath.Join(c.root(), "keep")) {
				t.Fatal("a sibling was deleted")
			}
			if runtime.GOOS != "windows" && !isDir(filepath.Join(c.root(), name)) {
				t.Fatal("the named folder was deleted")
			}
		})
	}
}

func TestAPackageNamedLikeAWindowsDrivePathIsRefused(t *testing.T) {
	c := installHome(t)
	c.serveRepo()
	sha := c.commit(map[string]string{"plugin.yaml": "name: 'D:outside'\n", "__init__.py": "",
		"desktop/plugin.js": "export default {}\n"})

	result := c.install(c.url, sha, "", true)

	refusedWith(t, result, "folder name")
	entries, err := os.ReadDir(c.root())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("root holds %v", entries)
	}
}

func TestACatalogPinNamingAnAnnotatedTagInstallsTheCommitItPointsAt(t *testing.T) {
	c := installHome(t)
	c.serveRepo()
	reviewed := c.commit(map[string]string{"plugin.js": "export default { v: 1 }\n"})
	gitIn(t, c.repo, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "tag", "-a", "v1.0", reviewed, "-m", "v1.0")
	c.commit(map[string]string{"plugin.js": "export default { v: 2 }\n"})
	tagObject := gitIn(t, c.repo, "rev-parse", "v1.0")
	if tagObject == reviewed {
		t.Fatal("the tag is not annotated")
	}
	c.catalog(tagObject)

	result := c.install(c.url, reviewed, "gizmo", false)

	okResult(t, result)
	if !strings.Contains(readText(t, filepath.Join(c.root(), "gizmo", "plugin.js")), "v: 1") {
		t.Fatal("not the commit the tag points at")
	}
}

func TestACatalogPinThatMovedSinceTheReviewIsRefused(t *testing.T) {
	c := installHome(t)
	c.serveRepo()
	reviewed := c.commit(map[string]string{"plugin.js": "export default { v: 1 }\n"})
	c.catalog(c.commit(map[string]string{"plugin.js": "export default { v: 2 }\n"}))

	result := c.install(c.url, reviewed, "gizmo", false)

	refusedWith(t, result, "inspect it again")
	if result["source_moved"] != true {
		t.Fatalf("result %v", result)
	}
	if exists(filepath.Join(c.root(), "gizmo")) {
		t.Fatal("a moved pin was published")
	}
}

func TestThePublishedPluginCarriesNoGitMetadata(t *testing.T) {
	c := installHome(t)
	c.serveRepo()
	sha := c.commit(map[string]string{"plugin.js": "export default {}\n"})

	okResult(t, c.install(c.url, sha, "", false))

	folder := filepath.Join(c.root(), "gizmo")
	if !isRegular(filepath.Join(folder, "plugin.js")) {
		t.Fatal("plugin.js not published")
	}
	if exists(filepath.Join(folder, ".git")) {
		t.Fatal("git metadata was published")
	}
}

// Read-only loose object in a read-only folder, what a hand cloned root level plugin carries
func readOnlyGitObjects(t *testing.T, folder string) string {
	t.Helper()
	obj := filepath.Join(folder, ".git", "objects", "ab", "cdef")
	writeFile(t, obj, "x")
	if err := os.Chmod(obj, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(obj), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(obj), 0o700) })
	return obj
}

func replaceThenRemoveAPluginHoldingReadOnlyGitObjects(t *testing.T) {
	c := installHome(t)
	c.serveRepo()
	sha := c.commit(map[string]string{"plugin.js": "export default { v: 1 }\n"})
	folder := filepath.Join(c.root(), "gizmo")
	okResult(t, c.install(c.url, sha, "", false))
	readOnlyGitObjects(t, folder)

	replaced := c.install(c.url, sha, "", true)

	okResult(t, replaced)
	if exists(filepath.Join(folder, ".git")) {
		t.Fatal("the replaced plugin kept its git objects")
	}

	readOnlyGitObjects(t, folder)
	result := c.remove("gizmo", false)

	okResult(t, result)
	if exists(folder) {
		t.Fatal("the plugin is still installed")
	}
}

func TestWindowsForceReplacesAndRemovesAPluginHoldingReadOnlyGitObjects(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("platforms windows")
	}
	replaceThenRemoveAPluginHoldingReadOnlyGitObjects(t)
}

func TestPosixForceReplacesAndRemovesAPluginHoldingReadOnlyGitObjects(t *testing.T) {
	posixOnly(t)
	if os.Geteuid() == 0 {
		t.Skip("root deletes from read-only folders")
	}
	replaceThenRemoveAPluginHoldingReadOnlyGitObjects(t)
}
