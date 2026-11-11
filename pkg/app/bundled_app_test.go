package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/selfupdate"
)

// Install stamp of a bundled desktop payload
func bundleStamp(overrides map[string]any) string {
	stamp := map[string]any{"schemaVersion": 2, "commit": strings.Repeat("a", 40), "commitDate": 1750000000, "branch": nil,
		"builtAt": "2026-08-21T00:00:00+00:00", "dirty": false, "source": "ci", "distribution": "desktop-app",
		"updateMechanism": "electron-updater", "baseVersion": "0.27.0", "displayVersion": "0.27.0", "distance": 0,
		"payload": "bundled", "tag": "v0.27.0"}
	for k, v := range overrides {
		stamp[k] = v
	}
	raw, _ := json.Marshal(stamp)
	return string(raw) + "\n"
}

// Executable file with a PE header
func bundleExe(t *testing.T, path string) string {
	return writeTestFile(t, path, "MZ", 0o755)
}

// Stages a payload repo under the app root and returns the repo root
func bundlePayload(t *testing.T, appRoot, resourcesName string) string {
	repo := filepath.Join(appRoot, resourcesName, "agent-payload", "repo")
	writeTestFile(t, filepath.Join(repo, "apps", "desktop", "package.json"), "{\"name\":\"desktop\"}\n", 0o644)
	writeTestFile(t, filepath.Join(repo, "install-stamp.json"), bundleStamp(nil), 0o644)
	return repo
}

func linuxBundle(t *testing.T, tmp string) string {
	app := filepath.Join(tmp, "linux-unpacked")
	repo := bundlePayload(t, app, "resources")
	bundleExe(t, filepath.Join(app, "Icebath"))
	bundleExe(t, filepath.Join(app, "chrome-sandbox"))
	bundleExe(t, filepath.Join(app, "chrome_crashpad_handler"))
	bundleExe(t, filepath.Join(app, "libffmpeg.so"))
	writeTestFile(t, filepath.Join(app, "version"), "40.9.3\n", 0o644)
	return repo
}

func windowsBundle(t *testing.T, tmp string) string {
	app := filepath.Join(tmp, "Icebath")
	repo := bundlePayload(t, app, "resources")
	bundleExe(t, filepath.Join(app, "Icebath.exe"))
	bundleExe(t, filepath.Join(app, "Uninstall Icebath.exe"))
	return repo
}

func macosBundle(t *testing.T, tmp string) string {
	app := filepath.Join(tmp, "Icebath.app")
	repo := bundlePayload(t, filepath.Join(app, "Contents"), "Resources")
	bundleExe(t, filepath.Join(app, "Contents", "MacOS", "Icebath"))
	return repo
}

// The host's own bundle shape and its launcher
func hostBundle(t *testing.T, tmp string) (string, string) {
	switch runtime.GOOS {
	case "windows":
		return windowsBundle(t, tmp), filepath.Join(tmp, "Icebath", "Icebath.exe")
	case "darwin":
		return macosBundle(t, tmp), filepath.Join(tmp, "Icebath.app", "Contents", "MacOS", "Icebath")
	}
	return linuxBundle(t, tmp), filepath.Join(tmp, "linux-unpacked", "Icebath")
}

func TestLinuxLauncherIsTheOneNonHelperExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Linux launcher rule reads POSIX execute bits")
	}
	tmp := t.TempDir()
	layout, err := resolveBundleLayout(linuxBundle(t, tmp), "linux")
	if err != nil {
		t.Fatal(err)
	}
	if layout.AppRoot != filepath.Join(tmp, "linux-unpacked") || filepath.Base(layout.Payload) != "agent-payload" ||
		layout.Launcher != filepath.Join(tmp, "linux-unpacked", "Icebath") {
		t.Fatalf("layout %+v", layout)
	}
}

func TestWindowsLauncherIsNeverTheUninstaller(t *testing.T) {
	tmp := t.TempDir()
	layout, err := resolveBundleLayout(windowsBundle(t, tmp), "win32")
	if err != nil || layout.Launcher != filepath.Join(tmp, "Icebath", "Icebath.exe") {
		t.Fatalf("layout %+v err %v", layout, err)
	}
}

func TestMacosAppRootClimbsOutOfContentsResources(t *testing.T) {
	tmp := t.TempDir()
	layout, err := resolveBundleLayout(macosBundle(t, tmp), "darwin")
	if err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(tmp, "Icebath.app")
	if layout.AppRoot != app || layout.Resources != filepath.Join(app, "Contents", "Resources") ||
		layout.Launcher != filepath.Join(app, "Contents", "MacOS", "Icebath") {
		t.Fatalf("layout %+v", layout)
	}
}

func TestAmbiguousLauncherReportsNoneRatherThanGuessing(t *testing.T) {
	tmp := t.TempDir()
	repo := windowsBundle(t, tmp)
	bundleExe(t, filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(repo))), "Other.exe"))
	layout, err := resolveBundleLayout(repo, "win32")
	if err != nil || layout.Launcher != "" {
		t.Fatalf("layout %+v err %v", layout, err)
	}
}

func TestACheckoutIsNotABundle(t *testing.T) {
	checkout := filepath.Join(t.TempDir(), "icebath-agent")
	if err := os.MkdirAll(filepath.Join(checkout, "apps", "desktop"), 0o755); err != nil {
		t.Fatal(err)
	}
	var nb *notBundledApp
	if _, err := resolveBundleLayout(checkout, ""); !errors.As(err, &nb) {
		t.Fatalf("err %v", err)
	}
}

func TestASealedTreeWithNoAppIsNotABundle(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "opt", "icebath", "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	var nb *notBundledApp
	if _, err := resolveBundleLayout(repo, ""); !errors.As(err, &nb) {
		t.Fatalf("err %v", err)
	}
}

func TestAStampedPayloadIsBundled(t *testing.T) {
	if !selfupdate.IsBundledPayload(linuxBundle(t, t.TempDir())) {
		t.Fatal("stamped payload not bundled")
	}
}

func TestASurvivingDesktopPackageJSONDoesNotMakeABundle(t *testing.T) {
	checkout := filepath.Join(t.TempDir(), "icebath-agent")
	writeTestFile(t, filepath.Join(checkout, "apps", "desktop", "package.json"), "{}\n", 0o644)
	if selfupdate.IsBundledPayload(checkout) {
		t.Fatal("checkout reported bundled")
	}
}

func TestABootstrapArtifactIsNotBundled(t *testing.T) {
	root := filepath.Join(t.TempDir(), "icebath-agent")
	writeTestFile(t, filepath.Join(root, "install-stamp.json"), bundleStamp(map[string]any{"payload": "bootstrap", "tag": nil}), 0o644)
	if selfupdate.IsBundledPayload(root) {
		t.Fatal("bootstrap reported bundled")
	}
}

func TestAnUnreadableStampIsNotBundled(t *testing.T) {
	root := filepath.Join(t.TempDir(), "icebath-agent")
	writeTestFile(t, filepath.Join(root, "install-stamp.json"), "{ not json\n", 0o644)
	if selfupdate.IsBundledPayload(root) {
		t.Fatal("unreadable stamp reported bundled")
	}
}

func TestDetachFlagsAndUnownedStdioAreForwarded(t *testing.T) {
	swapGUISteps(t)
	rec := &guiRecorder{}
	gui.popen = rec.popen
	pid, err := launchDetached([]string{"/app/Icebath", "--no-sandbox"}, nil, "/app")
	if err != nil || pid != 4242 || len(rec.popens) != 1 {
		t.Fatalf("pid %d err %v popens %d", pid, err, len(rec.popens))
	}
	spec := rec.popens[0]
	assertArgv(t, spec.Argv, []string{"/app/Icebath", "--no-sandbox"})
	if spec.Dir != "/app" || spec.Stdin != streamDevnull || spec.Stdout != streamDevnull || spec.Stderr != streamDevnull {
		t.Fatalf("spec %+v", spec)
	}
	if !detachRequested(spec.Attr) {
		t.Fatalf("no detach attributes %+v", spec.Attr)
	}
}

func TestARealDetachedChildRuns(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran.txt")
	argv, env := helperChild("marker", "ICEBATH_DESKTOP_TEST_MARKER", marker)
	pid, err := launchDetached(argv, env, "")
	if err != nil || pid <= 0 {
		t.Fatalf("pid %d err %v", pid, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if raw, _ := os.ReadFile(marker); string(raw) != "yes" {
		t.Fatalf("marker %q", raw)
	}
}

// Runs the launcher on a bundle with the build and spawn steps recorded
func runBundleGUI(t *testing.T, repo string, ns map[string]any, launcherOK, setuidUnusable bool, explicitProfile string) (int, [][]string, [][]string) {
	t.Helper()
	swapGUISteps(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	rec := &guiRecorder{}
	var builds [][]string
	gui.projectRoot = func() string { return repo }
	gui.run = func(spec guiRun) (*guiResult, error) { builds = append(builds, spec.Argv); return &guiResult{}, nil }
	gui.popen = rec.popen
	gui.sourceBuildEnv = func(_ context.Context, env map[string]string, _ bool) (map[string]string, error) {
		return copyEnv(env), nil
	}
	gui.buildNeeded = func(context.Context, string, string, bool) bool { return true }
	gui.prepare = func(_ context.Context, root string, workspaces []string, _ map[string]string, _ bool) error {
		builds = append(builds, append([]string{"npm", "ci", "--prefix", root}, workspaces...))
		return nil
	}
	gui.buildPrepared = func(_ context.Context, desktopDir string, _ bool, npm string, _ map[string]string) (string, error) {
		builds = append(builds, []string{"npm", "run", "builder", "--prefix", desktopDir})
		return "", nil
	}
	gui.sandboxFixup = func(io.Writer, string) bool { return launcherOK }
	gui.needsNoSandbox = func() bool { return !launcherOK }
	gui.helperIsRegularFile = func(string) bool { return true }
	gui.needsDisableSetuid = func(string) bool { return setuidUnusable }
	gui.passwordStore = func() string { return "" }
	gui.registerEntry = func(io.Writer, bool) *deferredDesktopEntryInstall { return nil }
	code, _ := runCmdGUI(t, ns, explicitProfile)
	var launches [][]string
	for _, spec := range rec.popens {
		launches = append(launches, spec.Argv)
	}
	return code, builds, launches
}

// Copy of an environment map
func copyEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}

func TestLaunchesTheBundleAndBuildsNothing(t *testing.T) {
	repo, launcher := hostBundle(t, t.TempDir())
	code, builds, launches := runBundleGUI(t, repo, guiNS(nil), true, false, "")
	if code != 0 || len(builds) != 0 || len(launches) != 1 {
		t.Fatalf("code %d builds %q launches %q", code, builds, launches)
	}
	assertArgv(t, launches[0], []string{launcher})
}

func TestANonSetuidHelperBesideAWorkingUsernsSandboxIsBypassed(t *testing.T) {
	repo, launcher := hostBundle(t, t.TempDir())
	code, _, launches := runBundleGUI(t, repo, guiNS(nil), true, true, "")
	if code != 0 || len(launches) != 1 {
		t.Fatalf("code %d launches %q", code, launches)
	}
	assertArgv(t, launches[0], []string{launcher, "--disable-setuid-sandbox"})
}

func TestExplicitProfileReachesTheBundledLauncher(t *testing.T) {
	tmp := t.TempDir()
	repo, launcher := hostBundle(t, filepath.Join(tmp, "app"))
	writeTestFile(t, filepath.Join(tmp, "icebath-root", "profiles", "work", "config.yaml"), "{}\n", 0o644)
	code, builds, launches := runBundleGUI(t, repo, guiNS(nil), true, false, "work")
	if code != 0 || len(builds) != 0 || len(launches) != 1 {
		t.Fatalf("code %d builds %q launches %q", code, builds, launches)
	}
	assertArgv(t, launches[0], []string{launcher, "--profile", "work"})
}

func TestACheckoutStillTakesTheBuildLadder(t *testing.T) {
	checkout := filepath.Join(t.TempDir(), "icebath-agent")
	writeTestFile(t, filepath.Join(checkout, "apps", "desktop", "package.json"), "{}\n", 0o644)
	_, builds, launches := runBundleGUI(t, checkout, guiNS(nil), true, false, "")
	if len(launches) != 0 {
		t.Fatalf("launches %q", launches)
	}
	found := false
	for _, cmd := range builds {
		if strings.Contains(strings.Join(cmd, " "), "npm") {
			found = true
		}
	}
	if !found {
		t.Fatalf("builds %q", builds)
	}
}

func TestBuildFlagsAreRefusedRatherThanIgnored(t *testing.T) {
	for _, flag := range []string{"source", "build_only", "force_build"} {
		t.Run(flag, func(t *testing.T) {
			repo, _ := hostBundle(t, t.TempDir())
			code, builds, launches := runBundleGUI(t, repo, guiNS(map[string]any{flag: true}), true, false, "")
			if code != 2 || len(builds) != 0 || len(launches) != 0 {
				t.Fatalf("code %d builds %q launches %q", code, builds, launches)
			}
		})
	}
}

func TestAStampedBundleWithNoAppReportsDamage(t *testing.T) {
	orphan := filepath.Join(t.TempDir(), "somewhere", "repo")
	writeTestFile(t, filepath.Join(orphan, "install-stamp.json"), bundleStamp(nil), 0o644)
	code, builds, launches := runBundleGUI(t, orphan, guiNS(nil), true, false, "")
	if code != 1 || len(builds) != 0 || len(launches) != 0 {
		t.Fatalf("code %d builds %q launches %q", code, builds, launches)
	}
}

func TestABundleWithNoResolvableLauncherReportsDamage(t *testing.T) {
	app := filepath.Join(t.TempDir(), "linux-unpacked")
	repo := bundlePayload(t, app, "resources")
	bundleExe(t, filepath.Join(app, "Icebath"))
	bundleExe(t, filepath.Join(app, "Icebath-Other"))
	code, builds, launches := runBundleGUI(t, repo, guiNS(nil), true, false, "")
	if code != 1 || len(builds) != 0 || len(launches) != 0 {
		t.Fatalf("code %d builds %q launches %q", code, builds, launches)
	}
}
