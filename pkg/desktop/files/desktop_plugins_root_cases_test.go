package files

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func markerOf(t *testing.T, dir string) map[string]any {
	t.Helper()
	v, err := jsonParse(readText(t, filepath.Join(dir, PackageMarker)))
	if err != nil {
		t.Fatal(err)
	}
	return v.(map[string]any)
}

// Records the warnings the passes log
func captureWarn(t *testing.T) *[]string {
	var mu sync.Mutex
	var got []string
	orig := warn
	warn = func(msg string) { mu.Lock(); got = append(got, msg); mu.Unlock() }
	t.Cleanup(func() { warn = orig })
	return &got
}

func reconcile(home, appRoot string) []string { return ReconcileUnifiedDesktopHalves(home, appRoot) }

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got, want)
	}
}

func setTimes(t *testing.T, p string, at time.Time) {
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateProfileScopedDesktopPluginsLiftsPerProfileDesktopPluginsIntoTheAppRootSoTheySurviveAProfileSwitch(t *testing.T) {
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	writeFile(t, filepath.Join(home, "profiles", "workbot", "desktop-plugins", "hello", "plugin.js"), "export default {}")
	os.MkdirAll(appRoot, 0o755)
	eq(t, MigrateProfileScopedDesktopPlugins(home, appRoot), []string{filepath.Join(appRoot, "hello")})
	if !exists(filepath.Join(appRoot, "hello", "plugin.js")) || exists(filepath.Join(home, "profiles", "workbot", "desktop-plugins")) {
		t.Fatal("not lifted")
	}
}

func TestMigrateProfileScopedDesktopPluginsNeverOverwritesAPluginAlreadyInstalledAtTheAppRoot(t *testing.T) {
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	writeFile(t, filepath.Join(appRoot, "hello", "plugin.js"), "root copy")
	scoped := filepath.Join(home, "profiles", "workbot", "desktop-plugins", "hello", "plugin.js")
	writeFile(t, scoped, "profile copy")
	eq(t, MigrateProfileScopedDesktopPlugins(home, appRoot), nil)
	if readText(t, filepath.Join(appRoot, "hello", "plugin.js")) != "root copy" || !exists(scoped) {
		t.Fatal("overwritten")
	}
}

func TestReconcileUnifiedDesktopHalvesCopiesEachUnifiedPackageDesktopHalfIntoTheAppRootONCEAcrossAllProfilesStampingThePackageMarker(t *testing.T) {
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	writeFile(t, filepath.Join(home, "plugins", "media", "plugin.yaml"), "name: media")
	writeFile(t, filepath.Join(home, "plugins", "media", "desktop", "plugin.js"), "default home copy")
	writeFile(t, filepath.Join(home, "profiles", "workbot", "plugins", "media", "desktop", "plugin.js"), "profile copy")
	writeFile(t, filepath.Join(home, "profiles", "workbot", "plugins", "snap", "plugin.yaml"), "name: snap")
	eq(t, reconcile(home, appRoot), []string{filepath.Join(appRoot, "media")})
	if readText(t, filepath.Join(appRoot, "media", "plugin.js")) != "default home copy" {
		t.Fatal("wrong home")
	}
	m := markerOf(t, filepath.Join(appRoot, "media"))
	if m["package"] != "media" || m["repo"] != nil || exists(filepath.Join(appRoot, "snap")) {
		t.Fatal(m)
	}
	eq(t, reconcile(home, appRoot), nil)
}

func TestReconcileUnifiedDesktopHalvesReCopiesWhenTheSourceHalfChangesAndRemovesTheCopyWhenThePackageIsUninstalled(t *testing.T) {
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	source := filepath.Join(home, "plugins", "media", "desktop", "plugin.js")
	writeFile(t, source, "v1")
	reconcile(home, appRoot)
	writeFile(t, source, "v2")
	setTimes(t, source, time.Now().Add(time.Minute))
	eq(t, reconcile(home, appRoot), []string{filepath.Join(appRoot, "media")})
	if readText(t, filepath.Join(appRoot, "media", "plugin.js")) != "v2" {
		t.Fatal("not recopied")
	}
	os.RemoveAll(filepath.Join(home, "plugins", "media"))
	eq(t, reconcile(home, appRoot), []string{filepath.Join(appRoot, "media")})
	if exists(filepath.Join(appRoot, "media")) {
		t.Fatal("ghost kept")
	}
}

func TestReconcileUnifiedDesktopHalvesMaterializesASYMLINKEDDevPackageAndReSyncsOnAnyByteChangeEvenWithAnOlderMtime(t *testing.T) {
	if !posix() {
		return
	}
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	checkout := filepath.Join(home, "src", "media")
	source := filepath.Join(checkout, "desktop", "plugin.js")
	writeFile(t, source, "v1")
	os.MkdirAll(filepath.Join(home, "plugins"), 0o755)
	mustSymlink(t, checkout, filepath.Join(home, "plugins", "media"))
	eq(t, reconcile(home, appRoot), []string{filepath.Join(appRoot, "media")})
	m := markerOf(t, filepath.Join(appRoot, "media"))
	if readText(t, filepath.Join(appRoot, "media", "plugin.js")) != "v1" || m["linked"] != true || m["package"] != "media" || m["source"] != filepath.Join(home, "plugins", "media", "desktop") {
		t.Fatal(m)
	}
	eq(t, reconcile(home, appRoot), nil)
	writeFile(t, source, "v2")
	setTimes(t, source, time.Now().Add(-time.Hour))
	eq(t, reconcile(home, appRoot), []string{filepath.Join(appRoot, "media")})
	if readText(t, filepath.Join(appRoot, "media", "plugin.js")) != "v2" {
		t.Fatal("not resynced")
	}
}

func TestReconcileUnifiedDesktopHalvesKeepsTheMtimeGateForAnInstalledNonLinkedPackage(t *testing.T) {
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	source := filepath.Join(home, "plugins", "media", "desktop", "plugin.js")
	writeFile(t, source, "v1")
	reconcile(home, appRoot)
	if _, ok := markerOf(t, filepath.Join(appRoot, "media"))["linked"]; ok {
		t.Fatal("linked stamped")
	}
	writeFile(t, source, "v2")
	setTimes(t, source, time.Now().Add(-time.Hour))
	eq(t, reconcile(home, appRoot), nil)
}

func TestReconcileUnifiedDesktopHalvesSkipsAPackageTheAppCannotReadAndStillMaterializesItsSiblings(t *testing.T) {
	if !unprivileged() {
		return
	}
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	denied := filepath.Join(home, "plugins", "denied", "desktop", "plugin.js")
	writeFile(t, denied, "x")
	writeFile(t, filepath.Join(home, "plugins", "good", "desktop", "plugin.js"), "y")
	os.Chmod(denied, 0)
	defer os.Chmod(denied, 0o600)
	warnings := captureWarn(t)
	eq(t, reconcile(home, appRoot), []string{filepath.Join(appRoot, "good")})
	if len(*warnings) == 0 {
		t.Fatal("no warning")
	}
}

func TestReconcileUnifiedDesktopHalvesKeepsAndWarnsAboutTheDesktopHalfOfAPackageFolderItCanNoLongerRead(t *testing.T) {
	if !unprivileged() {
		return
	}
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	denied := filepath.Join(home, "plugins", "denied")
	writeFile(t, filepath.Join(denied, "desktop", "plugin.js"), "x")
	reconcile(home, appRoot)
	if !exists(filepath.Join(appRoot, "denied")) {
		t.Fatal("not published")
	}
	os.Chmod(denied, 0)
	defer os.Chmod(denied, 0o700)
	warnings := captureWarn(t)
	eq(t, reconcile(home, appRoot), nil)
	if !exists(filepath.Join(appRoot, "denied")) || len(*warnings) == 0 {
		t.Fatal("pruned or silent")
	}
}

func TestReconcileUnifiedDesktopHalvesStampsThePackageOriginCatalogSidecarElseGitRemoteSoInstallHereCanReinstallTheAgentHalf(t *testing.T) {
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	writeFile(t, filepath.Join(home, "plugins", "cat", "desktop", "plugin.js"), "x")
	writeFile(t, filepath.Join(home, "plugins", "cat", ".hermes-catalog.json"), `{"catalog_name":"cat","repo":"https://github.com/o/cat.git","sha":"deadbeef"}`)
	writeFile(t, filepath.Join(home, "plugins", "raw", "desktop", "plugin.js"), "y")
	writeFile(t, filepath.Join(home, "plugins", "raw", ".git", "config"), "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = file:///srv/raw.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n")
	reconcile(home, appRoot)
	cat := markerOf(t, filepath.Join(appRoot, "cat"))
	if cat["catalogName"] != "cat" || cat["repo"] != "https://github.com/o/cat.git" || cat["sha"] != "deadbeef" {
		t.Fatal(cat)
	}
	if markerOf(t, filepath.Join(appRoot, "raw"))["repo"] != "file:///srv/raw.git" {
		t.Fatal("raw origin")
	}
}

func TestReconcileUnifiedDesktopHalvesCopiesARootLayoutPackagePluginJsAtThePackageRootAsTheInstallersPublishItMinusGitAndPycache(t *testing.T) {
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	pkg := filepath.Join(home, "plugins", "radar")
	writeFile(t, filepath.Join(pkg, "plugin.yaml"), "name: radar")
	writeFile(t, filepath.Join(pkg, "__init__.py"), "def register(ctx):\n    pass\n")
	writeFile(t, filepath.Join(pkg, "plugin.js"), "root entry")
	writeFile(t, filepath.Join(pkg, "panel.js"), "asset")
	writeFile(t, filepath.Join(pkg, "__pycache__", "x.pyc"), "pyc")
	writeFile(t, filepath.Join(pkg, ".git", "config"), "[remote \"origin\"]\n\turl = https://github.com/o/radar\n")
	leaky := filepath.Join(home, "plugins", "leaky")
	writeFile(t, filepath.Join(leaky, "plugin.js"), "x")
	writeFile(t, filepath.Join(home, "secret"), "s")
	mustSymlink(t, filepath.Join(home, "secret"), filepath.Join(leaky, "secret"))
	captureWarn(t)
	eq(t, reconcile(home, appRoot), []string{filepath.Join(appRoot, "radar")})
	radar := filepath.Join(appRoot, "radar")
	if readText(t, filepath.Join(radar, "plugin.js")) != "root entry" || readText(t, filepath.Join(radar, "panel.js")) != "asset" ||
		exists(filepath.Join(radar, ".git")) || exists(filepath.Join(radar, "__pycache__")) || exists(filepath.Join(appRoot, "leaky")) {
		t.Fatal("layout")
	}
	m := markerOf(t, radar)
	if m["package"] != "radar" || m["source"] != pkg || m["repo"] != "https://github.com/o/radar" {
		t.Fatal(m)
	}
	eq(t, reconcile(home, appRoot), nil)
	os.Remove(filepath.Join(pkg, "plugin.js"))
	eq(t, reconcile(home, appRoot), []string{radar})
	if exists(radar) {
		t.Fatal("not pruned")
	}
}

func TestReconcileUnifiedDesktopHalvesPublishesARealCopyOfALINKEDRootLayoutCheckoutKeepsItsMarkerOutOfTheCheckoutAndPrunesItWithTheLink(t *testing.T) {
	if !posix() {
		return
	}
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	checkout := filepath.Join(home, "src", "radar")
	writeFile(t, filepath.Join(checkout, "plugin.yaml"), "name: radar")
	writeFile(t, filepath.Join(checkout, "plugin.js"), "root entry")
	os.MkdirAll(filepath.Join(home, "plugins"), 0o755)
	link := filepath.Join(home, "plugins", "radar")
	mustSymlink(t, checkout, link)
	target := filepath.Join(appRoot, "radar")
	eq(t, reconcile(home, appRoot), []string{target})
	if st, _ := os.Lstat(target); isLinkMode(st.Mode()) || readText(t, filepath.Join(target, "plugin.js")) != "root entry" {
		t.Fatal("not a copy")
	}
	m := markerOf(t, target)
	if m["linked"] != true || m["package"] != "radar" || m["source"] != link || exists(filepath.Join(checkout, PackageMarker)) {
		t.Fatal(m)
	}
	eq(t, reconcile(home, appRoot), nil)
	os.Remove(link)
	eq(t, reconcile(home, appRoot), []string{target})
	if exists(target) || !reflect.DeepEqual(dirNames(t, checkout), []string{"plugin.js", "plugin.yaml"}) {
		t.Fatal("prune")
	}
}

func TestReconcileUnifiedDesktopHalvesReplacesAnEarlierCopyThatKeptTheCheckoutLinkAndDropsItOnceThePackageLinkIsGone(t *testing.T) {
	if !posix() {
		return
	}
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	checkout := filepath.Join(home, "src", "radar")
	link := filepath.Join(home, "plugins", "radar")
	target := filepath.Join(appRoot, "radar")
	writeFile(t, filepath.Join(checkout, "plugin.js"), "root entry")
	ghost := jsonStringify(NewOrderedObject("linked", true, "package", "radar", "source", link, "sourceMtimeMs", float64(time.Now().Add(time.Minute).UnixMilli())), "", "")
	writeFile(t, filepath.Join(checkout, PackageMarker), ghost)
	os.MkdirAll(filepath.Join(home, "plugins"), 0o755)
	os.MkdirAll(appRoot, 0o755)
	mustSymlink(t, checkout, link)
	mustSymlink(t, checkout, target)
	eq(t, reconcile(home, appRoot), []string{target})
	if st, _ := os.Lstat(target); isLinkMode(st.Mode()) || readText(t, filepath.Join(target, "plugin.js")) != "root entry" {
		t.Fatal("link kept")
	}
	os.RemoveAll(target)
	mustSymlink(t, checkout, target)
	os.Remove(link)
	eq(t, reconcile(home, appRoot), []string{target})
	if exists(target) || readText(t, filepath.Join(checkout, "plugin.js")) != "root entry" {
		t.Fatal("prune")
	}
}

func TestReconcileUnifiedDesktopHalvesNeverStampsAMarkerIntoAPluginTheUserLinkedIntoTheAppRoot(t *testing.T) {
	if !posix() {
		return
	}
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	own := filepath.Join(home, "src", "pkg")
	writeFile(t, filepath.Join(own, "plugin.js"), "same")
	writeFile(t, filepath.Join(home, "plugins", "pkg", "desktop", "plugin.js"), "same")
	os.MkdirAll(appRoot, 0o755)
	mustSymlink(t, own, filepath.Join(appRoot, "pkg"))
	eq(t, reconcile(home, appRoot), nil)
	if st, _ := os.Lstat(filepath.Join(appRoot, "pkg")); exists(filepath.Join(own, PackageMarker)) || !isLinkMode(st.Mode()) {
		t.Fatal("stamped")
	}
}

func TestReconcileUnifiedDesktopHalvesNeverOverwritesAStandalonePluginTheUserInstalledUnderTheSameName(t *testing.T) {
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	writeFile(t, filepath.Join(appRoot, "media", "plugin.js"), "user standalone")
	writeFile(t, filepath.Join(home, "plugins", "media", "desktop", "plugin.js"), "package half")
	eq(t, reconcile(home, appRoot), nil)
	if readText(t, filepath.Join(appRoot, "media", "plugin.js")) != "user standalone" {
		t.Fatal("overwritten")
	}
}

func TestReconcileUnifiedDesktopHalvesAdoptsAnUnmarkedCopyOfItsOwnHalfThatAPreMarkerInstallLeftBehind(t *testing.T) {
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	writeFile(t, filepath.Join(home, "plugins", "media", "desktop", "plugin.js"), "package half")
	writeFile(t, filepath.Join(appRoot, "media", "plugin.js"), "package half")
	eq(t, reconcile(home, appRoot), []string{filepath.Join(appRoot, "media")})
	if markerOf(t, filepath.Join(appRoot, "media"))["package"] != "media" || readText(t, filepath.Join(appRoot, "media", "plugin.js")) != "package half" {
		t.Fatal("adopt")
	}
	eq(t, reconcile(home, appRoot), nil)
}

func TestReconcileUnifiedDesktopHalvesReplacesAnInterruptedMarkerLessCopyStampsItAndConvergesOnRetry(t *testing.T) {
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	writeFile(t, filepath.Join(home, "plugins", "media", "desktop", "plugin.js"), "package half")
	writeFile(t, filepath.Join(appRoot, "media", "partial.js"), "interrupted copy")
	eq(t, reconcile(home, appRoot), []string{filepath.Join(appRoot, "media")})
	if readText(t, filepath.Join(appRoot, "media", "plugin.js")) != "package half" || exists(filepath.Join(appRoot, "media", "partial.js")) || !exists(filepath.Join(appRoot, "media", PackageMarker)) {
		t.Fatal("replace")
	}
	eq(t, reconcile(home, appRoot), nil)
}

func TestReconcileUnifiedDesktopHalvesOverlappingPassesPublishAHalfOnceFromTheFirstHomeThatHasIt(t *testing.T) {
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	writeFile(t, filepath.Join(home, "plugins", "media", "desktop", "plugin.js"), "default home copy")
	writeFile(t, filepath.Join(home, "profiles", "workbot", "plugins", "media", "desktop", "plugin.js"), "profile copy")
	warnings := captureWarn(t)
	var mu sync.Mutex
	var all []string
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := reconcile(home, appRoot)
			mu.Lock()
			all = append(all, got...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(*warnings) != 0 {
		t.Fatal(*warnings)
	}
	eq(t, all, []string{filepath.Join(appRoot, "media")})
	if readText(t, filepath.Join(appRoot, "media", "plugin.js")) != "default home copy" || !reflect.DeepEqual(dirNames(t, appRoot), []string{"media"}) {
		t.Fatal("overlap")
	}
	eq(t, reconcile(home, appRoot), nil)
}

func TestReconcileUnifiedDesktopHalvesCleansUpFailedStagingCopiesWithoutExposingAMarkerLessTarget(t *testing.T) {
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	packageDir := filepath.Join(home, "plugins", "media")
	writeFile(t, filepath.Join(packageDir, "desktop", "plugin.js"), "package half")
	orig := copyTreeFn
	copyTreeFn = func(_ string, dest string, _ func(string) bool) error {
		copyTreeFn = orig
		writeFile(t, filepath.Join(dest, "plugin.js"), "partial")
		return errors.New("disk full")
	}
	t.Cleanup(func() { copyTreeFn = orig })
	if _, err := MaterializeDesktopHalf(packageDir, appRoot, ""); err == nil || err.Error() != "disk full" {
		t.Fatal(err)
	}
	if exists(filepath.Join(appRoot, "media")) || len(dirNames(t, appRoot)) != 0 {
		t.Fatal(dirNames(t, appRoot))
	}
}

func TestDesktopTreePublicationRefusesAnInstalledCombinedPackageWhoseDesktopHalfLinksElsewhereInThePackage(t *testing.T) {
	if !posix() {
		return
	}
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	pkg := filepath.Join(home, "plugins", "media")
	writeFile(t, filepath.Join(pkg, "plugin.yaml"), "name: media\n")
	writeFile(t, filepath.Join(pkg, "assets", "asset.txt"), "package file")
	writeFile(t, filepath.Join(pkg, "desktop", "plugin.js"), "export default {}")
	mustSymlink(t, "../assets/asset.txt", filepath.Join(pkg, "desktop", "asset.txt"))
	warnings := captureWarn(t)
	eq(t, reconcile(home, appRoot), nil)
	if !strings.Contains(strings.Join(*warnings, " "), "symbolic link (asset.txt)") {
		t.Fatal(*warnings)
	}
	if entries, err := os.ReadDir(appRoot); err == nil && len(entries) != 0 {
		t.Fatal(entries)
	}
}

func TestDesktopTreePublicationRefusesAnInstalledPackageWhoseDesktopFolderIsALink(t *testing.T) {
	if !posix() {
		return
	}
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	writeFile(t, filepath.Join(home, "elsewhere", "plugin.js"), "export default {}")
	os.MkdirAll(filepath.Join(home, "plugins", "media"), 0o755)
	mustSymlink(t, filepath.Join(home, "elsewhere"), filepath.Join(home, "plugins", "media", "desktop"))
	_, err := MaterializeDesktopHalf(filepath.Join(home, "plugins", "media"), appRoot, "")
	if err == nil || !strings.Contains(err.Error(), "folder is a symbolic link") || exists(filepath.Join(appRoot, "media")) {
		t.Fatal(err)
	}
}

func TestDesktopTreePublicationPublishesADesktopFolderWhoseOnDiskNameDiffersOnlyInCase(t *testing.T) {
	if !posix() {
		return
	}
	keepPFS(t)
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	pkg := filepath.Join(home, "plugins", "media")
	writeFile(t, filepath.Join(pkg, "desktop", "plugin.js"), "export default {}")
	writeFile(t, filepath.Join(pkg, "Desktop", "plugin.js"), "export default {}")
	orig := pfs.realpath
	pfs.realpath = func(p string) (string, error) {
		if p == filepath.Join(pkg, "desktop") {
			return orig(filepath.Join(pkg, "Desktop"))
		}
		return orig(p)
	}
	got, err := MaterializeDesktopHalf(pkg, appRoot, "")
	if err != nil || got != filepath.Join(appRoot, "media") || readText(t, filepath.Join(appRoot, "media", "plugin.js")) != "export default {}" {
		t.Fatal(got, err)
	}
}

// Whether the temp folder ignores case, create Desktop and look for desktop
func caseInsensitiveTmp(t *testing.T) bool {
	probe := t.TempDir()
	os.Mkdir(filepath.Join(probe, "Desktop"), 0o755)
	return exists(filepath.Join(probe, "desktop"))
}

func TestDesktopTreePublicationPublishesADesktopFolderThatACaseInsensitiveVolumeFindsAsDesktop(t *testing.T) {
	if !caseInsensitiveTmp(t) {
		return
	}
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	writeFile(t, filepath.Join(home, "plugins", "media", "Desktop", "plugin.js"), "export default {}")
	report := ReconcileDesktopHalvesReport(home, appRoot)
	if !reflect.DeepEqual(report.Published, []string{filepath.Join(appRoot, "media")}) || len(report.Refused) != 0 {
		t.Fatal(report)
	}
}

func TestDesktopTreePublicationStillPublishesADevelopmentCheckoutTheUserLinkedInLinksInsideItIncluded(t *testing.T) {
	if !posix() {
		return
	}
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	checkout := filepath.Join(home, "src", "media")
	writeFile(t, filepath.Join(checkout, "desktop", "plugin.js"), "export default {}")
	writeFile(t, filepath.Join(checkout, "assets", "asset.txt"), "checkout file")
	mustSymlink(t, "../assets/asset.txt", filepath.Join(checkout, "desktop", "asset.txt"))
	os.MkdirAll(filepath.Join(home, "plugins"), 0o755)
	mustSymlink(t, checkout, filepath.Join(home, "plugins", "media"))
	eq(t, reconcile(home, appRoot), []string{filepath.Join(appRoot, "media")})
	if readText(t, filepath.Join(appRoot, "media", "asset.txt")) != "checkout file" {
		t.Fatal("asset")
	}
}

func TestDesktopTreePublicationNamesEachRefusedPackageAndItsReasonBesideTheHalvesItDidPublish(t *testing.T) {
	if !posix() {
		return
	}
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	pkg := filepath.Join(home, "plugins", "media")
	writeFile(t, filepath.Join(pkg, "assets", "asset.txt"), "package file")
	writeFile(t, filepath.Join(pkg, "desktop", "plugin.js"), "export default {}")
	mustSymlink(t, "../assets/asset.txt", filepath.Join(pkg, "desktop", "asset.txt"))
	writeFile(t, filepath.Join(home, "plugins", "good", "desktop", "plugin.js"), "export default {}")
	captureWarn(t)
	report := ReconcileDesktopHalvesReport(home, appRoot)
	want := DesktopReconcileReport{Published: []string{filepath.Join(appRoot, "good")}, Refused: []RefusedDesktopHalf{{Name: "media", Reason: "The desktop plugin contains a symbolic link (asset.txt); it is not installed."}}}
	if !reflect.DeepEqual(report, want) {
		t.Fatal(report)
	}
}

func TestDesktopTreePublicationDoesNotReportARefusalOnceAnotherProfilePublishedTheSamePackage(t *testing.T) {
	if !posix() {
		return
	}
	home := t.TempDir()
	appRoot := filepath.Join(home, "desktop-plugins")
	pkg := filepath.Join(home, "plugins", "media")
	writeFile(t, filepath.Join(pkg, "assets", "asset.txt"), "package file")
	writeFile(t, filepath.Join(pkg, "desktop", "plugin.js"), "export default {}")
	mustSymlink(t, "../assets/asset.txt", filepath.Join(pkg, "desktop", "asset.txt"))
	writeFile(t, filepath.Join(home, "profiles", "workbot", "plugins", "media", "desktop", "plugin.js"), "profile copy")
	captureWarn(t)
	report := ReconcileDesktopHalvesReport(home, appRoot)
	if !reflect.DeepEqual(report.Published, []string{filepath.Join(appRoot, "media")}) || len(report.Refused) != 0 {
		t.Fatal(report)
	}
}
