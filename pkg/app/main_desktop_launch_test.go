package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/selfupdate"
)

// Checkout with a desktop workspace manifest
func guiCheckout(t *testing.T, tmp string) string {
	root := filepath.Join(tmp, "icebath-agent")
	writeTestFile(t, filepath.Join(root, "apps", "desktop", "package.json"), "{}", 0o644)
	return root
}

// Packaged executable the release layout of this host carries
func guiPackagedExe(t *testing.T, root string) string {
	desktop := filepath.Join(root, "apps", "desktop", "release")
	var exe string
	switch runtime.GOOS {
	case "darwin":
		exe = filepath.Join(desktop, "mac-arm64", "Icebath.app", "Contents", "MacOS", "Icebath")
	case "windows":
		exe = filepath.Join(desktop, "win-unpacked", "Icebath.exe")
	default:
		exe = filepath.Join(desktop, "linux-unpacked", "icebath")
		writeTestFile(t, filepath.Join(filepath.Dir(exe), "chrome-sandbox"), "", 0o644)
	}
	return writeTestFile(t, exe, "", 0o755)
}

// Launcher steps for a checkout with no keychain probe, no entry and the launch env fixed
func guiLaunchFixture(t *testing.T, root string) {
	t.Helper()
	swapGUISteps(t)
	logging.ResetQueuedHandlers()
	t.Cleanup(logging.ResetQueuedHandlers)
	t.Setenv("ICEBATH_HOME", filepath.Join(filepath.Dir(root), "icebath-home"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(filepath.Dir(root), "xdg"))
	gui.projectRoot = func() string { return root }
	gui.launchEnv = func(context.Context, map[string]any) (map[string]string, []string, error) {
		return map[string]string{}, nil, nil
	}
	gui.registerEntry = func(io.Writer, bool) *deferredDesktopEntryInstall { return nil }
	gui.passwordStore = func() string { return "" }
}

func TestGuiReportsMissingSourceBeforeConstructingBuildLock(t *testing.T) {
	root := filepath.Join(t.TempDir(), "broken-icebath-agent")
	guiLaunchFixture(t, root)
	gui.newBuildLock = func(string) guiBuildLock { t.Fatal("build lock constructed before source validation"); return nil }
	code, out := runCmdGUI(t, map[string]any{}, "")
	if code != 1 || strings.TrimSpace(out) != "Desktop GUI source not found at: "+filepath.Join(root, "apps", "desktop") {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestGuiRefusesContendedBuildBeforeCheckingFreshness(t *testing.T) {
	root := guiCheckout(t, t.TempDir())
	guiLaunchFixture(t, root)
	holder := selfupdate.NewDesktopBuildLock(root)
	if ok, err := holder.Acquire(context.Background(), false); err != nil || !ok {
		t.Fatalf("holder %v %v", ok, err)
	}
	defer holder.Release(context.Background())
	gui.buildNeeded = func(context.Context, string, string, bool) bool {
		t.Fatal("freshness check ran without the build lock")
		return false
	}
	if code, out := runCmdGUI(t, guiNS(nil), ""); code != 2 {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestGuiReleasesLockBeforePackagedElectronHandoff(t *testing.T) {
	root := guiCheckout(t, t.TempDir())
	guiPackagedExe(t, root)
	guiLaunchFixture(t, root)
	gui.sandboxFixup = func(io.Writer, string) bool { return true }
	gui.needsDisableSetuid = func(string) bool { return false }
	gui.launchTarget = func(_ context.Context, _ string, exe string) string { return exe }
	gui.buildNeeded = func(context.Context, string, string, bool) bool { return false }
	gui.run = func(spec guiRun) (*guiResult, error) {
		probe := selfupdate.NewDesktopBuildLock(root)
		ok, err := probe.Acquire(context.Background(), false)
		if err != nil || !ok {
			t.Fatal("build lock was not released before the Electron handoff")
		}
		probe.Release(context.Background())
		return &guiResult{}, nil
	}
	if code, out := runCmdGUI(t, guiNS(nil), ""); code != 0 {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestGuiReleasesLockAfterBuildFailure(t *testing.T) {
	root := guiCheckout(t, t.TempDir())
	guiLaunchFixture(t, root)
	var prepared []string
	gui.sourceBuildEnv = func(context.Context, map[string]string, bool) (map[string]string, error) {
		return map[string]string{"PATH": ""}, nil
	}
	gui.prepare = func(_ context.Context, projectRoot string, _ []string, _ map[string]string, _ bool) error {
		prepared = append(prepared, projectRoot)
		return nil
	}
	gui.buildNeeded = func(context.Context, string, string, bool) bool { return true }
	gui.buildPrepared = func(context.Context, string, bool, string, map[string]string) (string, error) {
		return "", errors.New("tsc missing: ENOTEMPTY node_modules race")
	}
	code, out := runCmdGUI(t, guiNS(map[string]any{"build_only": true}), "")
	if code != 1 || !slices.Equal(prepared, []string{root}) || !strings.Contains(out, "✗ Desktop GUI build failed: tsc missing") {
		t.Fatalf("code %d prepared %v out %q", code, prepared, out)
	}
	successor := selfupdate.NewDesktopBuildLock(root)
	if ok, err := successor.Acquire(context.Background(), false); err != nil || !ok {
		t.Fatal("build lock was not released after a failed build")
	}
	successor.Release(context.Background())
}

func TestCurrentPackagedLaunchDoesNotRequireNpm(t *testing.T) {
	root := filepath.Join(t.TempDir(), "checkout")
	writeTestFile(t, filepath.Join(root, ".gitignore"), "dist/\nrelease/\nnode_modules/\n", 0o644)
	writeTestFile(t, filepath.Join(root, "package.json"), "{}", 0o644)
	writeTestFile(t, filepath.Join(root, "apps", "desktop", "package.json"), "{}", 0o644)
	writeTestFile(t, filepath.Join(root, "apps", "desktop", "app.ts"), "export const x = 1", 0o644)
	guiLaunchFixture(t, root)
	executable := filepath.Join(root, "apps", "desktop", "Icebath.exe")
	gui.packagedExecutable = func(string) (string, bool) { return executable, true }
	gui.buildNeeded = func(context.Context, string, string, bool) bool { return false }
	gui.launchTarget = func(_ context.Context, _ string, exe string) string { return exe }
	gui.sandboxFixup = func(io.Writer, string) bool { return true }
	gui.needsDisableSetuid = func(string) bool { return false }
	gui.sourceBuildEnv = func(context.Context, map[string]string, bool) (map[string]string, error) {
		t.Fatal("npm resolved for a current packaged launch")
		return nil, nil
	}
	runs := 0
	gui.run = func(guiRun) (*guiResult, error) { runs++; return &guiResult{}, nil }
	gui.platform = func() string { return "linux" }
	if code, out := runCmdGUI(t, map[string]any{"source": false, "skip_build": false, "force_build": false, "build_only": false}, ""); code != 0 || runs != 1 {
		t.Fatalf("code %d runs %d out %q", code, runs, out)
	}
}

func TestLauncherOptoutPreservesCustomEntryButCreatesMissing(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("platforms linux")
	}
	swapGUISteps(t)
	tmp := t.TempDir()
	h := filepath.Join(tmp, "home")
	_ = os.MkdirAll(h, 0o755)
	t.Setenv("HOME", h)
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "xdg"))
	t.Setenv("PATH", "/usr/bin:/bin")
	root := filepath.Join(tmp, "checkout")
	_ = os.MkdirAll(root, 0o755)
	entry := filepath.Join(tmp, "xdg", "applications", desktopEntryFileName)
	custom := "[Desktop Entry]\nType=Application\nName=Custom Icebath\nExec=/opt/custom-icebath desktop\n"
	for _, setting := range []string{"false", "\"false\""} {
		writeTestFile(t, filepath.Join(h, "config.yaml"), "desktop:\n  manage_launcher_entry: "+setting+"\n", 0o644)
		writeTestFile(t, entry, custom, 0o644)
		if got := ldeInstall(t, root); got != entry {
			t.Fatalf("entry %s", got)
		}
		if raw, _ := os.ReadFile(entry); string(raw) != custom {
			t.Fatalf("entry rewritten %q", raw)
		}
	}
	_ = os.Remove(entry)
	if got := ldeInstall(t, root); got != entry {
		t.Fatalf("missing entry not created: %s", got)
	}
}

// Packaged launch command of a checkout launched with an explicit profile flag
func profileLaunchCommand(t *testing.T, explicitProfile string) []string {
	t.Helper()
	tmp := t.TempDir()
	root := guiCheckout(t, tmp)
	exe := guiPackagedExe(t, root)
	guiLaunchFixture(t, root)
	gui.buildNeeded = func(context.Context, string, string, bool) bool { return false }
	gui.launchTarget = func(_ context.Context, _ string, e string) string { return e }
	gui.sandboxFixup = func(io.Writer, string) bool { return true }
	gui.needsDisableSetuid = func(string) bool { return false }
	var calls [][]string
	gui.run = func(spec guiRun) (*guiResult, error) { calls = append(calls, spec.Argv); return &guiResult{}, nil }
	if code, out := runCmdGUI(t, guiNS(map[string]any{"skip_build": true}), explicitProfile); code != 0 {
		t.Fatalf("code %d out %q", code, out)
	}
	var launches [][]string
	for _, cmd := range calls {
		if len(cmd) > 0 && cmd[0] == exe {
			launches = append(launches, cmd)
		}
	}
	if len(launches) != 1 {
		t.Fatalf("launches %q", launches)
	}
	return launches[0]
}

func TestDashPDesktopAppendsProfileToPackagedLaunch(t *testing.T) {
	if launched := profileLaunchCommand(t, "desktop"); !slices.Equal(launched[len(launched)-2:], []string{"--profile", "desktop"}) {
		t.Fatalf("launched %q", launched)
	}
}

func TestProfileEqualsSpellingIsForwarded(t *testing.T) {
	if launched := profileLaunchCommand(t, "work"); !slices.Equal(launched[len(launched)-2:], []string{"--profile", "work"}) {
		t.Fatalf("launched %q", launched)
	}
}

func TestMissingFlagDoesNotForwardStickyProfile(t *testing.T) {
	if launched := profileLaunchCommand(t, ""); slices.Contains(launched, "--profile") {
		t.Fatalf("launched %q", launched)
	}
}

func TestSourceLaunchReadsBomElectronPathWithoutProvisioning(t *testing.T) {
	for _, local := range []bool{false, true} {
		tmp := t.TempDir()
		root := guiCheckout(t, tmp)
		writeTestFile(t, filepath.Join(root, "apps", "desktop", "dist", "index.html"), "prepared renderer", 0o644)
		electron := filepath.Join(root, "node_modules", "electron")
		writeTestFile(t, filepath.Join(electron, "package.json"), "{}", 0o644)
		executable := writeTestFile(t, filepath.Join(electron, "dist", "électron"), "", 0o755)
		writeTestFile(t, filepath.Join(electron, "path.txt"), "\ufeff"+filepath.Base(executable)+"\n", 0o644)
		guiLaunchFixture(t, root)
		var calls [][]string
		gui.run = func(spec guiRun) (*guiResult, error) { calls = append(calls, spec.Argv); return &guiResult{}, nil }
		ns := guiNS(map[string]any{"source": true, "skip_build": true, "local": local})
		code, out := runCmdGUI(t, ns, "")
		want := []string{executable, "."}
		if local {
			want = append(want, "--local")
		}
		if code != 0 || len(calls) != 1 || !slices.Equal(calls[0], want) {
			t.Fatalf("local %v code %d calls %q out %q", local, code, calls, out)
		}
		_ = os.Remove(executable)
		if code, _ := runCmdGUI(t, ns, ""); code != 1 || len(calls) != 1 {
			t.Fatalf("missing runtime code %d calls %q", code, calls)
		}
	}
}

func TestSitePackagesInstallKindDetectsBrewAndPip(t *testing.T) {
	cases := map[string]string{
		"/opt/homebrew/Cellar/icebath-agent/0.1/libexec/lib/python3.12/site-packages": "homebrew",
		"/usr/lib/python3/dist-packages":                                              "pip",
		"/home/u/.venv/lib/python3.12/site-packages":                                  "pip",
		"/home/u/src/icebath-agent":                                                   "",
	}
	for root, want := range cases {
		if got := sitePackagesInstallKind(root); got != want {
			t.Fatalf("%s got %q want %q", root, got, want)
		}
	}
}

func TestGuiForegroundLaunchCtrlCExitsCleanly(t *testing.T) {
	root := guiCheckout(t, t.TempDir())
	guiPackagedExe(t, root)
	guiLaunchFixture(t, root)
	gui.platform = func() string { return "linux" }
	gui.buildNeeded = func(context.Context, string, string, bool) bool { return false }
	gui.launchTarget = func(_ context.Context, _ string, e string) string { return e }
	gui.sandboxFixup = func(io.Writer, string) bool { return true }
	gui.needsDisableSetuid = func(string) bool { return false }
	gui.run = func(guiRun) (*guiResult, error) { return &guiResult{Interrupted: true}, nil }
	code, out := runCmdGUI(t, guiNS(nil), "")
	if code != 0 || !strings.Contains(out, "Icebath Desktop closed.") {
		t.Fatalf("code %d out %q", code, out)
	}
}

// Homebrew site packages tree with no desktop source
func brewRoot(t *testing.T) string {
	root := filepath.Join(t.TempDir(), "Cellar", "icebath-agent", "2026.7.7.2", "libexec", "site-packages")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestGuiBrewInstallPrintsBrewGuidanceNotVenvHint(t *testing.T) {
	root := brewRoot(t)
	guiLaunchFixture(t, root)
	gui.launchInstalledMac = func(io.Writer) (bool, error) { return false, nil }
	code, out := runCmdGUI(t, guiNS(nil), "")
	if code != 1 || !strings.Contains(out, "Desktop GUI source not found") || !strings.Contains(out, "Homebrew") {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestGuiBrewInstallLaunchesInstalledAppWhenPresent(t *testing.T) {
	root := brewRoot(t)
	guiLaunchFixture(t, root)
	launched := 0
	gui.launchInstalledMac = func(io.Writer) (bool, error) { launched++; return true, nil }
	if code, _ := runCmdGUI(t, guiNS(nil), ""); code != 0 || launched != 1 {
		t.Fatalf("code %d launched %d", code, launched)
	}
}

func TestGuiClosePreviewFlagForwardsToPackagedExe(t *testing.T) {
	root := guiCheckout(t, t.TempDir())
	exe := guiPackagedExe(t, root)
	guiLaunchFixture(t, root)
	gui.buildNeeded = func(context.Context, string, string, bool) bool { return false }
	gui.launchTarget = func(_ context.Context, _ string, e string) string { return e }
	gui.sandboxFixup = func(io.Writer, string) bool { return true }
	var launched [][]string
	gui.run = func(spec guiRun) (*guiResult, error) {
		launched = append(launched, spec.Argv)
		return &guiResult{}, nil
	}
	code, _ := runCmdGUI(t, guiNS(map[string]any{"close_preview": true}), "")
	want := []string{exe, "--close-preview"}
	if runtime.GOOS == "linux" {
		want = []string{exe, "--disable-setuid-sandbox", "--close-preview"}
	}
	if code != 0 || len(launched) != 1 || !slices.Equal(launched[0], want) {
		t.Fatalf("code %d launched %q", code, launched)
	}
}

func TestLaunchInstalledMacosDesktopAppGatesOnBundleAndPlatform(t *testing.T) {
	for _, c := range []struct {
		exists   bool
		platform string
	}{{true, "darwin"}, {false, "darwin"}, {true, "linux"}} {
		swapGUISteps(t)
		gui.platform = func() string { return c.platform }
		exe := filepath.Join(t.TempDir(), "Icebath.app", "Contents", "MacOS", "Icebath")
		if c.exists {
			writeTestFile(t, exe, "", 0o755)
		}
		prev := installedMacosAppExecutable
		installedMacosAppExecutable = exe
		var calls [][]string
		gui.popen = func(spec guiRun) (int, error) { calls = append(calls, spec.Argv); return 4321, nil }
		launched, err := launchInstalledMacosDesktopApp(io.Discard)
		installedMacosAppExecutable = prev
		want := c.exists && c.platform == "darwin"
		if err != nil || launched != want || (want && (len(calls) != 1 || !slices.Equal(calls[0], []string{exe}))) {
			t.Fatalf("%+v launched %v calls %v err %v", c, launched, calls, err)
		}
	}
}
