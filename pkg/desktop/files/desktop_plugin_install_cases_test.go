package files

import (
	"crypto/rand"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestResolvePluginGitUrlMapsOwnerRepoShorthandToGithubGitUrl(t *testing.T) {
	got, err := ResolvePluginGitURL("NousResearch/hermes-example-plugins")
	if err != nil || got != (ResolvedGitURL{GitURL: "https://github.com/NousResearch/hermes-example-plugins.git"}) {
		t.Fatal(got, err)
	}
}

func TestResolvePluginGitUrlSupportsMonorepoSubdirShorthand(t *testing.T) {
	got, err := ResolvePluginGitURL("owner/repo/plugins/foo")
	if err != nil || got != (ResolvedGitURL{GitURL: "https://github.com/owner/repo.git", Subdir: "plugins/foo"}) {
		t.Fatal(got, err)
	}
}

func TestResolvePluginGitUrlSupportsHashSubdirFragment(t *testing.T) {
	got, err := ResolvePluginGitURL("https://github.com/o/r.git#nested/plugin")
	if err != nil || got != (ResolvedGitURL{GitURL: "https://github.com/o/r.git", Subdir: "nested/plugin"}) {
		t.Fatal(got, err)
	}
}

func TestDesktopPluginFolderNameUsesTheRepoNameForARootLevelPluginNotTheClonePath(t *testing.T) {
	if got := DesktopPluginFolderName("https://github.com/o/my-plugin.git", ""); got != "my-plugin" {
		t.Fatal(got)
	}
}

func TestDesktopPluginFolderNameUsesTheLastMeaningfulSubdirNotAGenericDesktopFolder(t *testing.T) {
	if got := DesktopPluginFolderName("https://github.com/o/monorepo.git", "plugins/alerts/desktop"); got != "alerts" {
		t.Fatal(got)
	}
}

func TestResolveSubdirWithinRejectsPathTraversal(t *testing.T) {
	if _, err := ResolveSubdirWithin(t.TempDir(), "../escape"); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatal(err)
	}
}

func TestFindDesktopEntryFindsRootPluginJs(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "desktop"), 0o755)
	writeFile(t, filepath.Join(root, "plugin.js"), "export default {}")
	if got := FindDesktopEntry(root); got == nil || *got != (DesktopEntry{EntryFile: filepath.Join(root, "plugin.js"), SourceSubdir: "."}) {
		t.Fatal(got)
	}
}

func TestFindDesktopEntryFindsDesktopPluginJs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "desktop", "plugin.js"), "export default {}")
	if got := FindDesktopEntry(root); got == nil || *got != (DesktopEntry{EntryFile: filepath.Join(root, "desktop", "plugin.js"), SourceSubdir: "desktop"}) {
		t.Fatal(got)
	}
}

func TestDetectPluginComponentsDetectsAManifestOnlyPackageDashboardBackendNoInitPy(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugin.yaml"), "name: bulletin\n")
	writeFile(t, filepath.Join(root, "dashboard", "manifest.json"), `{"name":"bulletin","api":"plugin_api.py"}`+"\n")
	writeFile(t, filepath.Join(root, "desktop", "plugin.js"), `export default { id: "bulletin" }`)
	d := DetectPluginComponents(root)
	if !d.Agent || !d.Desktop || d.AgentName != "bulletin" {
		t.Fatal(d)
	}
}

func TestDetectPluginComponentsDetectsAgentOnlyLayout(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugin.yaml"), "name: hello-agent\n")
	writeFile(t, filepath.Join(root, "__init__.py"), "def register(ctx): pass\n")
	d := DetectPluginComponents(root)
	if !d.Agent || d.Desktop || d.AgentName != "hello-agent" {
		t.Fatal(d)
	}
}

func TestDetectPluginComponentsDetectsDualLayout(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugin.yaml"), "name: dual\n")
	writeFile(t, filepath.Join(root, "__init__.py"), "def register(ctx): pass\n")
	writeFile(t, filepath.Join(root, "desktop", "plugin.js"), `export default { id: "dual-ui" }`)
	d := DetectPluginComponents(root)
	if !d.Agent || !d.Desktop || d.AgentName != "dual" || d.DesktopName != "desktop" {
		t.Fatal(d)
	}
}

// Git in a hermetic home, the commits of a fixture repository authored by Fixture
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.com",
		"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(args, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func gitHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func commitAll(t *testing.T, repo, msg string) {
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-qm", msg)
}

// A repository whose root is one plugin, desktop/plugin.js plus the agent half when agentName is set
func pluginRepo(t *testing.T, agentName string, desktopFiles map[string]string) string {
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "desktop", "plugin.js"), "export function register() {}\n")
	for name, text := range desktopFiles {
		writeFile(t, filepath.Join(repo, "desktop", name), text)
	}
	if agentName != "" {
		writeFile(t, filepath.Join(repo, "plugin.yaml"), "name: "+agentName+"\n")
		writeFile(t, filepath.Join(repo, "__init__.py"), "def register(ctx): pass\n")
	}
	gitIn(t, repo, "init", "-q")
	commitAll(t, repo, "init")
	return repo
}

func TestProbePluginRepoProbesAMonorepoSubdirectoryThroughTheSparsePartialClone(t *testing.T) {
	gitHome(t)
	repo := t.TempDir()
	plugin := filepath.Join(repo, "integrations", "icebath")
	writeFile(t, filepath.Join(plugin, "plugin.yaml"), "name: nested-agent\n")
	writeFile(t, filepath.Join(plugin, "__init__.py"), "def register(ctx): pass\n")
	writeFile(t, filepath.Join(repo, "unrelated.bin"), strings.Repeat("x", 4096))
	gitIn(t, repo, "init", "-q")
	gitIn(t, repo, "config", "uploadpack.allowFilter", "true")
	commitAll(t, repo, "init")
	got := plain(ProbePluginRepo("git", pathToFileURL(repo)+"#integrations/icebath"))
	if got["ok"] != true || got["agent"] != true || got["agentName"] != "nested-agent" {
		t.Fatal(got)
	}
}

func install(t *testing.T, repo, appRoot string, force bool, sha *string) map[string]any {
	return plain(InstallDesktopPluginFromGit("git", pathToFileURL(repo), appRoot, force, sha))
}

func TestInstallDesktopPluginFromGitInstallsTheCommitTheDialogReviewedAfterTheBranchMovedAndRefusesOneTheSourceLacks(t *testing.T) {
	gitHome(t)
	repo := pluginRepo(t, "", map[string]string{"panel.js": "reviewed\n"})
	appRoot := t.TempDir()
	probe := plain(ProbePluginRepo("git", pathToFileURL(repo)))
	if probe["sha"] != gitIn(t, repo, "rev-parse", "HEAD") {
		t.Fatal(probe)
	}
	writeFile(t, filepath.Join(repo, "desktop", "panel.js"), "moved\n")
	commitAll(t, repo, "moved")
	sha := probe["sha"].(string)
	result := install(t, repo, appRoot, false, &sha)
	name := jsString(result["pluginName"])
	if result["ok"] != true || readText(t, filepath.Join(appRoot, name, "panel.js")) != "reviewed\n" {
		t.Fatal(result)
	}
	bogus := strings.Repeat("f", 40)
	refused := install(t, repo, appRoot, true, &bogus)
	if refused["ok"] != false || refused["sourceMoved"] != true || !strings.Contains(jsString(refused["error"]), "the source may have changed since you reviewed it") ||
		readText(t, filepath.Join(appRoot, name, "panel.js")) != "reviewed\n" {
		t.Fatal(refused)
	}
}

func TestInstallDesktopPluginFromGitInstallsAReviewedCatalogPinThatNamesAnAnnotatedTagAtTheCommitItPointsAt(t *testing.T) {
	gitHome(t)
	repo := pluginRepo(t, "", map[string]string{"panel.js": "reviewed\n"})
	gitIn(t, repo, "tag", "-a", "v1.0", "-m", "v1.0")
	writeFile(t, filepath.Join(repo, "desktop", "panel.js"), "moved\n")
	commitAll(t, repo, "moved")
	tagObject := gitIn(t, repo, "rev-parse", "v1.0")
	if tagObject == gitIn(t, repo, "rev-parse", "v1.0^{commit}") {
		t.Fatal("tag not annotated")
	}
	appRoot := t.TempDir()
	result := install(t, repo, appRoot, false, &tagObject)
	if result["ok"] != true || readText(t, filepath.Join(appRoot, jsString(result["pluginName"]), "panel.js")) != "reviewed\n" {
		t.Fatal(result)
	}
}

func TestInstallDesktopPluginFromGitRefusesADesktopTreeWithAndPublishesNothing(t *testing.T) {
	for _, tc := range []struct {
		label string
		link  func(repo, outside string)
	}{
		{"a symlinked file inside it", func(repo, _ string) { os.Symlink("/etc/hostname", filepath.Join(repo, "desktop", "leak.txt")) }},
		{"a symlinked desktop folder", func(repo, outside string) {
			os.Rename(filepath.Join(repo, "desktop"), filepath.Join(outside, "desktop"))
			os.Symlink(filepath.Join(outside, "desktop"), filepath.Join(repo, "desktop"))
		}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			gitHome(t)
			repo := pluginRepo(t, "", nil)
			outside, appRoot := t.TempDir(), t.TempDir()
			tc.link(repo, outside)
			commitAll(t, repo, "link")
			result := install(t, repo, appRoot, false, nil)
			if result["ok"] != false || !strings.Contains(jsString(result["error"]), "symbolic link") || len(dirNames(t, appRoot)) != 0 {
				t.Fatal(result)
			}
		})
	}
}

func TestInstallDesktopPluginFromGitKeepsTheInstalledPluginWhenAForcedReinstallRefusesTheNewTree(t *testing.T) {
	gitHome(t)
	repo := pluginRepo(t, "", map[string]string{"panel.js": "working\n"})
	appRoot := t.TempDir()
	installed := install(t, repo, appRoot, false, nil)
	name := jsString(installed["pluginName"])
	if installed["ok"] != true {
		t.Fatal(installed)
	}
	os.Symlink("/etc/hostname", filepath.Join(repo, "desktop", "leak.txt"))
	commitAll(t, repo, "link")
	refused := install(t, repo, appRoot, true, nil)
	if refused["ok"] != false || !strings.Contains(jsString(refused["error"]), "symbolic link") || !reflect.DeepEqual(dirNames(t, appRoot), []string{name}) ||
		readText(t, filepath.Join(appRoot, name, "panel.js")) != "working\n" || !exists(filepath.Join(appRoot, name, "plugin.js")) {
		t.Fatal(refused)
	}
	os.Remove(filepath.Join(repo, "desktop", "leak.txt"))
	writeFile(t, filepath.Join(repo, "desktop", "panel.js"), "replaced\n")
	commitAll(t, repo, "plain")
	if got := install(t, repo, appRoot, true, nil); got["ok"] != true || !reflect.DeepEqual(dirNames(t, appRoot), []string{name}) ||
		readText(t, filepath.Join(appRoot, name, "panel.js")) != "replaced\n" {
		t.Fatal(got)
	}
}

func TestInstallDesktopPluginFromGitLeavesTheClonesGitOutOfARootLayoutHalfAndOutOfItsSizeCap(t *testing.T) {
	gitHome(t)
	repo, appRoot := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(repo, "plugin.js"), "export function register() {}\n")
	payload := make([]byte, 36*1024*1024)
	rand.Read(payload)
	os.WriteFile(filepath.Join(repo, "asset.bin"), payload, 0o644)
	gitIn(t, repo, "init", "-q")
	commitAll(t, repo, "init")
	result := install(t, repo, appRoot, false, nil)
	if result["ok"] != true || !reflect.DeepEqual(dirNames(t, filepath.Join(appRoot, jsString(result["pluginName"]))), []string{"asset.bin", "plugin.js"}) {
		t.Fatal(result)
	}
}

func TestInstallDesktopPluginFromGitRefusesAPackageNamedOnAForcedInstallAndDeletesNothing(t *testing.T) {
	gitHome(t)
	for _, name := range []string{"..", "C:..", "a/b", "nul .js", "C:foo", ".hidden", "gizmo.", strings.Repeat("x", 129)} {
		t.Run(name, func(t *testing.T) {
			repo := pluginRepo(t, name, nil)
			home := t.TempDir()
			appRoot := filepath.Join(home, "desktop-plugins")
			os.Mkdir(appRoot, 0o755)
			writeFile(t, filepath.Join(home, "config.yaml"), "keep\n")
			result := install(t, repo, appRoot, true, nil)
			if readText(t, filepath.Join(home, "config.yaml")) != "keep\n" || result["ok"] != false ||
				!strings.Contains(jsString(result["error"]), "is not a desktop plugin folder name") || len(dirNames(t, appRoot)) != 0 {
				t.Fatal(result)
			}
		})
	}
}

func manifestRepo(t *testing.T, name string) string {
	repo := pluginRepo(t, "", nil)
	body, _ := json.Marshal(map[string]string{"name": name})
	writeFile(t, filepath.Join(repo, "plugin.json"), string(body))
	commitAll(t, repo, "manifest")
	return repo
}

func TestInstallDesktopPluginFromGitRefusesAPluginJsonNameWithALongInnerRunOfSpacesWithoutStalling(t *testing.T) {
	gitHome(t)
	repo := manifestRepo(t, "a"+strings.Repeat(" ", 200_000)+"b")
	appRoot := t.TempDir()
	start := time.Now()
	result := install(t, repo, appRoot, false, nil)
	if time.Since(start) > 10*time.Second || result["ok"] != false || !strings.Contains(jsString(result["error"]), "is not a desktop plugin folder name") || len(dirNames(t, appRoot)) != 0 {
		t.Fatal(result)
	}
}

func TestInstallDesktopPluginFromGitNamesThePackageMarkerAfterTheFolderWhenAPluginJsonNameHasOuterSpaces(t *testing.T) {
	gitHome(t)
	repo := manifestRepo(t, " spaced ")
	appRoot := t.TempDir()
	result := install(t, repo, appRoot, false, nil)
	if result["ok"] != true || result["pluginName"] != "spaced" || markerOf(t, filepath.Join(appRoot, "spaced"))["package"] != "spaced" {
		t.Fatal(result)
	}
}

func TestInstallDesktopPluginFromGitInstallsUnderTheFolderTheAgentSidePicks(t *testing.T) {
	gitHome(t)
	for _, tc := range [][3]string{
		{"a non-BMP name over 128 UTF-16 units", strings.Repeat("\U0001F600", 50) + strings.Repeat("x", 29), strings.Repeat("\U0001F600", 50) + strings.Repeat("x", 29)},
		{"a BOM-prefixed name", "\ufeffgizmo", "\ufeffgizmo"},
		{"a name wrapped in NEL and U+001C", "\u0085gizmo\x1c", "gizmo"},
	} {
		t.Run(tc[0], func(t *testing.T) {
			repo := manifestRepo(t, tc[1])
			appRoot := t.TempDir()
			result := install(t, repo, appRoot, false, nil)
			if result["ok"] != true || result["pluginName"] != tc[2] || !reflect.DeepEqual(dirNames(t, appRoot), []string{tc[2]}) ||
				markerOf(t, filepath.Join(appRoot, tc[2]))["package"] != tc[2] {
				t.Fatal(result)
			}
		})
	}
}

func TestInstallDesktopPluginFromGitStampsThePackageMarkerOnAUnifiedPackageHalfAndNamesTheFolderAfterTheAgentPackage(t *testing.T) {
	gitHome(t)
	repo := pluginRepo(t, "hermes-talk", nil)
	appRoot := t.TempDir()
	result := install(t, repo, appRoot, false, nil)
	m := markerOf(t, filepath.Join(appRoot, "hermes-talk"))
	if result["ok"] != true || result["pluginName"] != "hermes-talk" || m["package"] != "hermes-talk" || m["repo"] != pathToFileURL(repo) {
		t.Fatal(result, m)
	}
}

func TestInstallDesktopPluginFromGitKeepsAGitInstalledUnifiedHalfWhenNoLocalAgentPackageExists(t *testing.T) {
	gitHome(t)
	repo := pluginRepo(t, "hermes-talk", nil)
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	result := install(t, repo, appRoot, false, nil)
	published := filepath.Join(appRoot, "hermes-talk")
	m := markerOf(t, published)
	if result["ok"] != true || m["source"] != published || !exists(filepath.Join(published, "plugin.js")) {
		t.Fatal(result, m)
	}
	touched := reconcile(home, appRoot)
	if !exists(filepath.Join(published, "plugin.js")) || contains(touched, published) {
		t.Fatal(touched)
	}
}

func TestInstallDesktopPluginFromGitLetsALocalAgentPackageReplaceTheGitInstalledHalfOnReconcile(t *testing.T) {
	gitHome(t)
	repo := pluginRepo(t, "hermes-talk", nil)
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	install(t, repo, appRoot, false, nil)
	packageDesktop := filepath.Join(home, "plugins", "hermes-talk", "desktop")
	writeFile(t, filepath.Join(packageDesktop, "plugin.js"), "from the agent package\n")
	reconcile(home, appRoot)
	if readText(t, filepath.Join(appRoot, "hermes-talk", "plugin.js")) != "from the agent package\n" || markerOf(t, filepath.Join(appRoot, "hermes-talk"))["source"] != packageDesktop {
		t.Fatal("not replaced")
	}
}

func TestInstallDesktopPluginFromGitLeavesADesktopOnlyRepoUnmarkedSoItStaysAStandalonePlugin(t *testing.T) {
	gitHome(t)
	repo := pluginRepo(t, "", nil)
	appRoot := t.TempDir()
	result := install(t, repo, appRoot, false, nil)
	dir := filepath.Join(appRoot, jsString(result["pluginName"]))
	if result["ok"] != true || !exists(filepath.Join(dir, "plugin.js")) || exists(filepath.Join(dir, PackageMarker)) {
		t.Fatal(result)
	}
}

func TestInstallDesktopPluginFromGitDropsAPackageMarkerADesktopOnlyRepoShipsSoItCannotPoseAsAnotherPackage(t *testing.T) {
	gitHome(t)
	repo := pluginRepo(t, "", map[string]string{PackageMarker: `{"package":"meshy-ish","source":"/elsewhere"}`})
	appRoot := t.TempDir()
	result := install(t, repo, appRoot, false, nil)
	dir := filepath.Join(appRoot, jsString(result["pluginName"]))
	if result["ok"] != true || !exists(filepath.Join(dir, "plugin.js")) || exists(filepath.Join(dir, PackageMarker)) {
		t.Fatal(result)
	}
}
