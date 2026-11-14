package app

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Isolated XDG data home, home dir, empty PATH and Linux host for the entry installer
func ldeFixture(t *testing.T) (string, string) {
	t.Helper()
	swapGUISteps(t)
	tmp := t.TempDir()
	xdg := filepath.Join(tmp, "xdg-data")
	t.Setenv("XDG_DATA_HOME", xdg)
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "icebath-home"))
	emptyBin := filepath.Join(tmp, "empty-bin")
	_ = os.MkdirAll(emptyBin, 0o755)
	t.Setenv("PATH", emptyBin)
	gui.platform = func() string { return "linux" }
	lde.refreshDatabases = func(string) []string { return nil }
	savedArgv := relaunchArgv
	t.Cleanup(func() { relaunchArgv = savedArgv })
	clear(cliImportProbeCache)
	return tmp, xdg
}

// Checkout with a fake PNG icon
func ldeProject(t *testing.T, tmp string) string {
	root := filepath.Join(tmp, "icebath-agent")
	writeTestFile(t, filepath.Join(root, "apps", "desktop", "assets", "icon.png"), "\x89PNG fake", 0o644)
	return root
}

// Key value pairs of a desktop entry
func ldeParse(text string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "[") {
			values[k] = v
		}
	}
	return values
}

// Installs the entry and returns its path, failing on an error
func ldeInstall(t *testing.T, root string) string {
	t.Helper()
	entry, err := installDesktopEntry(root)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

// Parsed values of an installed entry
func ldeValues(t *testing.T, entry string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	return ldeParse(string(raw))
}

// Resolver answering a fixed launcher
func ldeResolve(path string) func() (string, bool) {
	return func() (string, bool) { return path, path != "" }
}

// Resolver answering argv zero when it is set
func ldeResolveArgv0() (string, bool) {
	argv := relaunchArgv()
	if len(argv) > 0 && argv[0] != "" {
		return argv[0], true
	}
	return "", false
}

// Sets argv to the launch context argv zero
func ldeArgv0(argv0 string) {
	argv := []string{argv0, "desktop"}
	relaunchArgv = func() []string { return argv }
}

// Executable script with text
func ldeScript(t *testing.T, path, text string) string { return writeTestFile(t, path, text, 0o755) }

// PNG prefix whose IHDR carries the size and no pixels
func pngIHDR(w, h uint32) string {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	_ = binary.Write(&b, binary.BigEndian, [2]uint32{w, h})
	return b.String()
}

// Puts a fake icebath launcher on PATH
func ldeOnPath(t *testing.T, wrapper string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pathbin")
	_ = os.MkdirAll(dir, 0o755)
	if err := os.Symlink(wrapper, filepath.Join(dir, "icebath")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestInstallWritesEntryWithAbsoluteExecAndIcon(t *testing.T) {
	tmp, xdg := ldeFixture(t)
	root := ldeProject(t, tmp)
	bin := writeTestFile(t, filepath.Join(tmp, "bin", "icebath"), "", 0o644)
	lde.resolveBin = ldeResolve(bin)
	lde.installIcon = func(string) bool { return false }
	entry := ldeInstall(t, root)
	values := ldeValues(t, entry)
	if entry != filepath.Join(xdg, "applications", desktopEntryFileName) || values["Exec"] != bin+" desktop" ||
		!filepath.IsAbs(values["Icon"]) || values["Icon"] != desktopIconPath(root) {
		t.Fatalf("entry %s values %v", entry, values)
	}
}

func TestInstallPrefersThemedIconFromHicolor(t *testing.T) {
	tmp, xdg := ldeFixture(t)
	root := ldeProject(t, tmp)
	lde.resolveBin = ldeResolve(writeTestFile(t, filepath.Join(tmp, "bin", "icebath"), "", 0o644))
	values := ldeValues(t, ldeInstall(t, root))
	dest := filepath.Join(xdg, "icons", "hicolor", "256x256", "apps", "icebath.png")
	got, _ := os.ReadFile(dest)
	want, _ := os.ReadFile(desktopIconPath(root))
	if values["Icon"] != "icebath" || !bytes.Equal(got, want) {
		t.Fatalf("values %v dest %q", values, got)
	}
}

func TestInstallIconCopyFailureFallsBackToAbsolute(t *testing.T) {
	tmp, _ := ldeFixture(t)
	root := ldeProject(t, tmp)
	lde.resolveBin = ldeResolve(writeTestFile(t, filepath.Join(tmp, "bin", "icebath"), "", 0o644))
	lde.copyfile = func(string, string) error { return &os.PathError{Op: "open", Path: "x", Err: os.ErrPermission} }
	values := ldeValues(t, ldeInstall(t, root))
	if values["Icon"] != desktopIconPath(root) || values["Type"] != "Application" || values["Name"] != "Icebath" || values["Terminal"] != "false" {
		t.Fatalf("values %v", values)
	}
}

func TestInstalledEntryIsExecutable(t *testing.T) {
	tmp, _ := ldeFixture(t)
	lde.resolveBin = ldeResolve("/usr/bin/icebath")
	st, err := os.Stat(ldeInstall(t, ldeProject(t, tmp)))
	if err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("mode %v err %v", st, err)
	}
}

func TestExecFallsBackToInterpreterModule(t *testing.T) {
	tmp, _ := ldeFixture(t)
	lde.resolveBin = ldeResolve("")
	exec := ldeValues(t, ldeInstall(t, ldeProject(t, tmp)))["Exec"]
	if !strings.HasSuffix(exec, "-m icebath_cli.main desktop") || !filepath.IsAbs(strings.Trim(strings.Split(exec, " ")[0], "\"")) {
		t.Fatalf("exec %q", exec)
	}
}

func TestExecPrefixesInterpreterForEnvShebangPythonScript(t *testing.T) {
	tmp, _ := ldeFixture(t)
	bin := ldeScript(t, filepath.Join(tmp, "bin", "icebath"), "#!/usr/bin/env python3\nimport icebath_cli\n")
	lde.resolveBin = ldeResolve(bin)
	lde.canImportCLI = func(string) bool { return true }
	exec := ldeValues(t, ldeInstall(t, ldeProject(t, tmp)))["Exec"]
	if strings.Trim(strings.Split(exec, " ")[0], "\"") != runningInterpreter() || !strings.Contains(exec, bin) || !strings.HasSuffix(exec, "desktop") {
		t.Fatalf("exec %q", exec)
	}
}

func TestExecLeavesShellWrapperLaunchersAlone(t *testing.T) {
	tmp, _ := ldeFixture(t)
	bin := ldeScript(t, filepath.Join(tmp, "bin", "icebath"), "#!/usr/bin/env bash\nexec /opt/icebath/venv/bin/python \"$@\"\n")
	lde.resolveBin = ldeResolve(bin)
	if exec := ldeValues(t, ldeInstall(t, ldeProject(t, tmp)))["Exec"]; exec != bin+" desktop" {
		t.Fatalf("exec %q", exec)
	}
}

func TestExecLeavesVenvShebangScriptsAlone(t *testing.T) {
	tmp, _ := ldeFixture(t)
	bin := ldeScript(t, filepath.Join(tmp, "bin", "icebath"), "#!"+lde.executable()+"\nimport icebath_cli\n")
	lde.resolveBin = ldeResolve(bin)
	if exec := ldeValues(t, ldeInstall(t, ldeProject(t, tmp)))["Exec"]; exec != bin+" desktop" {
		t.Fatalf("exec %q", exec)
	}
}

func TestExecConvergesFromRepoScriptArgv0ToInstalledWrapper(t *testing.T) {
	tmp, _ := ldeFixture(t)
	root := ldeProject(t, tmp)
	repoScript := ldeScript(t, filepath.Join(root, "icebath"), "#!/usr/bin/env python3\nimport icebath_cli\n")
	wrapper := ldeScript(t, filepath.Join(tmp, "installed", "bin", "icebath"), "#!/usr/bin/env bash\nexec "+lde.executable()+" \"$@\"\n")
	ldeArgv0(repoScript)
	ldeOnPath(t, wrapper)
	lde.resolveBin = resolveHermesBin
	exec := ldeValues(t, ldeInstall(t, root))["Exec"]
	if exec != wrapper+" desktop" {
		t.Fatalf("exec %q", exec)
	}
}

func TestExecNeverPersistsABareInterpreterCommand(t *testing.T) {
	tmp, _ := ldeFixture(t)
	root := ldeProject(t, tmp)
	wrapper := ldeScript(t, filepath.Join(tmp, "installed", "bin", "icebath"), "#!/usr/bin/env bash\nexit 0\n")
	interpreter := writeTestFile(t, filepath.Join(tmp, "uv", "cpython-3.11.15", "bin", "python3.11"), "\x7fELF fake", 0o755)
	ldeArgv0(interpreter)
	ldeOnPath(t, wrapper)
	lde.resolveBin = resolveHermesBin
	exec := ldeValues(t, ldeInstall(t, root))["Exec"]
	first := strings.Trim(strings.Split(exec, " ")[0], "\"")
	if first == interpreter || strings.HasPrefix(filepath.Base(first), "python") || exec != wrapper+" desktop" {
		t.Fatalf("exec %q", exec)
	}
}

func TestExecKeepsResolverFallbackWhenNoWrapperOnPath(t *testing.T) {
	tmp, _ := ldeFixture(t)
	root := ldeProject(t, tmp)
	repoScript := ldeScript(t, filepath.Join(root, "icebath"), "#!/usr/bin/env python3\nimport icebath_cli\n")
	ldeArgv0(repoScript)
	lde.resolveBin = ldeResolveArgv0
	exec := ldeValues(t, ldeInstall(t, root))["Exec"]
	if !strings.HasSuffix(exec, "-m icebath_cli.main desktop") || !filepath.IsAbs(strings.Trim(strings.Split(exec, " ")[0], "\"")) || strings.Contains(exec, repoScript) {
		t.Fatalf("exec %q", exec)
	}
}

// Known wrapper execing the checkout's script
func ldeCheckoutWrapper(t *testing.T, path, root string) string {
	return ldeScript(t, path, "#!/usr/bin/env bash\nexec "+filepath.Join(root, "venv", "bin", "python")+" "+filepath.Join(root, "icebath")+" \"$@\"\n")
}

func TestExecUsesKnownWrapperWhenPathLookupMisses(t *testing.T) {
	tmp, _ := ldeFixture(t)
	root := ldeProject(t, tmp)
	repoScript := ldeScript(t, filepath.Join(root, "icebath"), "#!/usr/bin/env python3\nimport icebath_cli\n")
	known := ldeCheckoutWrapper(t, filepath.Join(tmp, "known-home", ".local", "bin", "icebath"), root)
	t.Setenv("HOME", filepath.Join(tmp, "known-home"))
	ldeArgv0(repoScript)
	lde.resolveBin = ldeResolveArgv0
	if exec := ldeValues(t, ldeInstall(t, root))["Exec"]; exec != known+" desktop" {
		t.Fatalf("exec %q", exec)
	}
}

func TestExecNeverPersistsACheckoutInternalPathHit(t *testing.T) {
	tmp, _ := ldeFixture(t)
	root := ldeProject(t, tmp)
	venvScript := ldeScript(t, filepath.Join(root, "venv", "bin", "icebath"), "#!/usr/bin/env bash\nexec true\n")
	known := ldeCheckoutWrapper(t, filepath.Join(tmp, "path-home", ".local", "bin", "icebath"), root)
	t.Setenv("HOME", filepath.Join(tmp, "path-home"))
	lde.resolveBin = func() (string, bool) {
		if argv0, ok := ldeResolveArgv0(); ok {
			return argv0, true
		}
		return venvScript, true
	}
	ldeArgv0(venvScript)
	entry := ldeInstall(t, root)
	first, _ := os.ReadFile(entry)
	exec := ldeParse(string(first))["Exec"]
	if exec != known+" desktop" || strings.Contains(exec, venvScript) {
		t.Fatalf("exec %q", exec)
	}
	clear(cliImportProbeCache)
	second, _ := os.ReadFile(ldeInstall(t, root))
	if !bytes.Equal(first, second) {
		t.Fatalf("entry changed %q %q", first, second)
	}
}

func TestExecSkipsManagedEnvironmentCliWithoutDesktop(t *testing.T) {
	tmp, _ := ldeFixture(t)
	root := ldeProject(t, tmp)
	installs := filepath.Join(tmp, "installs")
	lde.installStateEnvsRoot = func(r string) string { return filepath.Join(installs, pm.InstallKey(r), "environments") }
	generation := filepath.Join(installs, pm.InstallKey(root), "environments", "gen")
	_ = os.MkdirAll(filepath.Join(generation, "workspace"), 0o755)
	managed := ldeScript(t, filepath.Join(generation, "venv", "bin", "icebath"), "#!/usr/bin/env bash\nexec true\n")
	known := ldeCheckoutWrapper(t, filepath.Join(tmp, ".local", "bin", "icebath"), root)
	lde.resolveBin = ldeResolve(managed)
	ldeArgv0(managed)
	exec := ldeValues(t, ldeInstall(t, root))["Exec"]
	if exec != known+" desktop" || strings.Contains(exec, managed) {
		t.Fatalf("exec %q", exec)
	}
}

func TestExecFindsKnownWrapperWhenResolverHasNoCandidate(t *testing.T) {
	tmp, _ := ldeFixture(t)
	root := ldeProject(t, tmp)
	known := ldeCheckoutWrapper(t, filepath.Join(tmp, "cold-home", ".local", "bin", "icebath"), root)
	t.Setenv("HOME", filepath.Join(tmp, "cold-home"))
	ldeArgv0("-c")
	lde.resolveBin = ldeResolve("")
	entry := ldeInstall(t, root)
	first, _ := os.ReadFile(entry)
	if exec := ldeParse(string(first))["Exec"]; exec != known+" desktop" {
		t.Fatalf("exec %q", exec)
	}
	clear(cliImportProbeCache)
	second, _ := os.ReadFile(ldeInstall(t, root))
	if !bytes.Equal(first, second) {
		t.Fatal("entry changed")
	}
}

func TestExecRejectsKnownWrapperFromAnotherCheckout(t *testing.T) {
	tmp, _ := ldeFixture(t)
	root := ldeProject(t, tmp)
	repoScript := ldeScript(t, filepath.Join(root, "icebath"), "#!/usr/bin/env python3\nimport icebath_cli\n")
	other := filepath.Join(tmp, "other-install")
	_ = os.MkdirAll(other, 0o755)
	foreign := ldeCheckoutWrapper(t, filepath.Join(tmp, "known-home", ".local", "bin", "icebath"), other)
	t.Setenv("HOME", filepath.Join(tmp, "known-home"))
	ldeArgv0(repoScript)
	lde.resolveBin = ldeResolveArgv0
	exec := ldeValues(t, ldeInstall(t, root))["Exec"]
	if strings.Contains(exec, foreign) || !strings.HasSuffix(exec, "-m icebath_cli.main desktop") {
		t.Fatalf("exec %q", exec)
	}
}

func TestKnownWrapperCandidatesCoverInstallerLayouts(t *testing.T) {
	sentinel := "/home/__sentinel_home__"
	for _, c := range []struct {
		layout, prefix string
		euid           int
		expected       string
	}{
		{"user", "", 1000, sentinel + "/.local/bin/icebath"},
		{"termux", "PREFIX-SET-BY-TEST", 1000, "PREFIX-SET-BY-TEST/bin/icebath"},
		{"root-fhs", "", 0, "/usr/local/bin/icebath"},
		{"non-root-no-fhs", "", 1000, sentinel + "/.local/bin/icebath"},
	} {
		swapGUISteps(t)
		t.Setenv("HOME", sentinel)
		t.Setenv("PREFIX", c.prefix)
		euid := c.euid
		lde.geteuid = func() int { return euid }
		candidates := knownWrapperCandidates()
		if !slices.Contains(candidates, c.expected) ||
			(c.layout == "termux" && candidates[0] != c.expected) ||
			(c.layout == "root-fhs" && slices.Index(candidates, "/usr/local/bin/icebath") > slices.Index(candidates, sentinel+"/.local/bin/icebath")) ||
			(c.layout == "non-root-no-fhs" && slices.Contains(candidates, "/usr/local/bin/icebath")) {
			t.Fatalf("%s candidates %v", c.layout, candidates)
		}
	}
}

// Fixed resolver for a plain install
func ldeFixedInstall(t *testing.T, tmp string) {
	lde.resolveBin = ldeResolve(writeTestFile(t, filepath.Join(tmp, "bin", "icebath"), "", 0o644))
}

func TestInstalledEntryCarriesTheWindowAppID(t *testing.T) {
	tmp, _ := ldeFixture(t)
	ldeFixedInstall(t, tmp)
	entry := ldeInstall(t, ldeProject(t, tmp))
	values := ldeValues(t, entry)
	if filepath.Base(entry) != desktopAppID+".desktop" || values["StartupWMClass"] != desktopAppID || values["Name"] != "Icebath" {
		t.Fatalf("entry %s values %v", entry, values)
	}
}

const ldeLegacyText = "[Desktop Entry]\nType=Application\nName=Icebath\nExec=icebath desktop\n"

func TestInstallKeepsTheLegacyEntryAsAHiddenAlias(t *testing.T) {
	tmp, xdg := ldeFixture(t)
	ldeFixedInstall(t, tmp)
	root := ldeProject(t, tmp)
	legacy := writeTestFile(t, filepath.Join(xdg, "applications", legacyDesktopEntryFileName), ldeLegacyText, 0o644)
	entry := ldeInstall(t, root)
	alias := ldeValues(t, legacy)
	values := ldeValues(t, entry)
	if alias["NoDisplay"] != "true" || alias["StartupWMClass"] != desktopAppID || alias["Exec"] != values["Exec"] || alias["Icon"] != values["Icon"] {
		t.Fatalf("alias %v entry %v", alias, values)
	}
}

func TestUnchangedEntryStillAliasesALegacyEntry(t *testing.T) {
	tmp, xdg := ldeFixture(t)
	ldeFixedInstall(t, tmp)
	root := ldeProject(t, tmp)
	ldeInstall(t, root)
	legacy := writeTestFile(t, filepath.Join(xdg, "applications", legacyDesktopEntryFileName), ldeLegacyText, 0o644)
	ldeInstall(t, root)
	if ldeValues(t, legacy)["NoDisplay"] != "true" {
		t.Fatal("legacy entry not aliased")
	}
}

func TestInstallKeepsForeignFilesAtTheLegacyPath(t *testing.T) {
	tmp, xdg := ldeFixture(t)
	ldeFixedInstall(t, tmp)
	foreign := writeTestFile(t, filepath.Join(xdg, "applications", legacyDesktopEntryFileName),
		"[Desktop Entry]\nType=Application\nName=Someone else\nExec=other-app\n", 0o644)
	ldeInstall(t, ldeProject(t, tmp))
	if raw, _ := os.ReadFile(foreign); !strings.Contains(string(raw), "Name=Someone else") {
		t.Fatalf("foreign %q", raw)
	}
}

func TestInstallOptOutPreservesTheLegacyEntry(t *testing.T) {
	tmp, xdg := ldeFixture(t)
	home := filepath.Join(tmp, "icebath-home-opt")
	writeTestFile(t, filepath.Join(home, "config.yaml"), "desktop:\n  manage_launcher_entry: false\n", 0o644)
	t.Setenv("ICEBATH_HOME", home)
	ldeFixedInstall(t, tmp)
	legacy := writeTestFile(t, filepath.Join(xdg, "applications", legacyDesktopEntryFileName), ldeLegacyText, 0o644)
	entry := ldeInstall(t, ldeProject(t, tmp))
	raw, _ := os.ReadFile(legacy)
	if entry != filepath.Join(xdg, "applications", desktopEntryFileName) || string(raw) != ldeLegacyText {
		t.Fatalf("entry %s legacy %q", entry, raw)
	}
}

func TestAppIDMatchesTheDesktopBuildIdentity(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not available")
	}
	repo, _ := filepath.Abs("../..")
	cmd := exec.Command(node, "-e", "console.log(require('./apps/desktop/product-identity.cjs').appId)")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("product-identity.cjs did not evaluate: %v", err)
	}
	if strings.TrimSpace(string(out)) != desktopAppID {
		t.Fatalf("app id %q", out)
	}
}

func TestInstallIsIdempotentAndSkipsCacheRefresh(t *testing.T) {
	tmp, _ := ldeFixture(t)
	lde.resolveBin = ldeResolve("/usr/bin/icebath")
	var calls []string
	lde.refreshDatabases = func(d string) []string { calls = append(calls, d); return nil }
	root := ldeProject(t, tmp)
	ldeInstall(t, root)
	if len(calls) != 1 {
		t.Fatalf("calls %v", calls)
	}
	ldeInstall(t, root)
	if len(calls) != 1 {
		t.Fatalf("calls %v", calls)
	}
}

func TestInstallWithoutSourceIconUsesThemedName(t *testing.T) {
	tmp, _ := ldeFixture(t)
	root := filepath.Join(tmp, "icebath-agent")
	_ = os.MkdirAll(root, 0o755)
	lde.resolveBin = ldeResolve("/usr/bin/icebath")
	if icon := ldeValues(t, ldeInstall(t, root))["Icon"]; icon != "icebath" {
		t.Fatalf("icon %q", icon)
	}
}

func TestInstallIsANoopOnMacos(t *testing.T) {
	tmp, _ := ldeFixture(t)
	gui.platform = func() string { return "darwin" }
	if entry := ldeInstall(t, ldeProject(t, tmp)); entry != "" {
		t.Fatalf("entry %s", entry)
	}
}

func TestInstallIsANoopOnWindows(t *testing.T) {
	tmp, _ := ldeFixture(t)
	gui.platform = func() string { return "win32" }
	if entry := ldeInstall(t, ldeProject(t, tmp)); entry != "" {
		t.Fatalf("entry %s", entry)
	}
}

// Tools reported present and every quiet run recorded and answered by ok
func ldeFakeTools(available []string, ok func([]string) bool) *[][]string {
	ran := &[][]string{}
	gui.which = func(name string) string {
		if slices.Contains(available, name) {
			return "/usr/bin/" + name
		}
		return ""
	}
	lde.runQuiet = func(cmd []string, _ time.Duration, _ string) (bool, bool) {
		*ran = append(*ran, cmd)
		return ok(cmd), true
	}
	return ran
}

func TestRefreshFallsBackToKbuildsycoca5(t *testing.T) {
	swapGUISteps(t)
	ran := ldeFakeTools([]string{"kbuildsycoca5"}, func([]string) bool { return true })
	tools := refreshDesktopDatabases(t.TempDir())
	if !slices.Equal(tools, []string{"kbuildsycoca5"}) || len(*ran) != 1 || !slices.Equal((*ran)[0], []string{"/usr/bin/kbuildsycoca5", "--noincremental"}) {
		t.Fatalf("tools %v ran %v", tools, *ran)
	}
}

func TestRefreshPrefersKbuildsycoca6Over5(t *testing.T) {
	swapGUISteps(t)
	ran := ldeFakeTools([]string{"kbuildsycoca6", "kbuildsycoca5"}, func([]string) bool { return true })
	refreshDesktopDatabases(t.TempDir())
	if len(*ran) != 1 || (*ran)[0][0] != "/usr/bin/kbuildsycoca6" {
		t.Fatalf("ran %v", *ran)
	}
}

func TestRefreshSkipsMissingTools(t *testing.T) {
	swapGUISteps(t)
	ran := ldeFakeTools(nil, func([]string) bool { return true })
	if tools := refreshDesktopDatabases(t.TempDir()); len(tools) != 0 || len(*ran) != 0 {
		t.Fatalf("tools %v ran %v", tools, *ran)
	}
}

func TestRefreshReportsOnlyToolsThatSucceeded(t *testing.T) {
	swapGUISteps(t)
	gui.which = func(name string) string { return "/usr/bin/" + name }
	lde.runQuiet = func(cmd []string, _ time.Duration, _ string) (bool, bool) {
		return strings.Contains(cmd[0], "kbuildsycoca"), true
	}
	if tools := refreshDesktopDatabases(t.TempDir()); !slices.Equal(tools, []string{"kbuildsycoca6"}) {
		t.Fatalf("tools %v", tools)
	}
}

func TestRunQuietSwallowsMissingBinary(t *testing.T) {
	if ok, _ := runQuiet([]string{filepath.Join(t.TempDir(), "definitely-not-a-binary")}, 60*time.Second, ""); ok {
		t.Fatal("missing binary succeeded")
	}
}

func TestRunningInterpreterKeepsVenvSemanticPath(t *testing.T) {
	swapGUISteps(t)
	tmp := t.TempDir()
	base := writeTestFile(t, filepath.Join(tmp, "base", "python3.11"), "", 0o755)
	writeTestFile(t, filepath.Join(tmp, "venv", "pyvenv.cfg"), "home = /base\n", 0o644)
	venvPython := filepath.Join(tmp, "venv", "bin", "python")
	_ = os.MkdirAll(filepath.Dir(venvPython), 0o755)
	if err := os.Symlink(base, venvPython); err != nil {
		t.Skip("symlinks unavailable")
	}
	lde.executable = func() string { return venvPython }
	if got := runningInterpreter(); got != venvPython {
		t.Fatalf("venv interpreter %q", got)
	}
	plain := filepath.Join(tmp, "plain", "bin", "python3")
	_ = os.MkdirAll(filepath.Dir(plain), 0o755)
	_ = os.Symlink(base, plain)
	lde.executable = func() string { return plain }
	resolvedBase, _ := filepath.EvalSymlinks(base)
	if got := runningInterpreter(); got != resolvedBase {
		t.Fatalf("plain interpreter %q want %q", got, resolvedBase)
	}
}

func TestRunningInterpreterResolvesPlainInterpreter(t *testing.T) {
	swapGUISteps(t)
	lde.executable = func() string { return "/usr/bin/python3" }
	if got := runningInterpreter(); !filepath.IsAbs(got) {
		t.Fatalf("interpreter %q", got)
	}
}

func TestExecFallsBackToRunningInterpreterWhenProbeFails(t *testing.T) {
	tmp, _ := ldeFixture(t)
	root := ldeProject(t, tmp)
	interpreter := writeTestFile(t, filepath.Join(tmp, "uv", "bin", "python3.11"), "\x7fELF fake", 0o755)
	ldeArgv0(interpreter)
	lde.resolveBin = ldeResolveArgv0
	lde.canImportCLI = func(string) bool { return false }
	exec := ldeValues(t, ldeInstall(t, root))["Exec"]
	first := strings.Trim(strings.Split(exec, " ")[0], "\"")
	if !strings.HasSuffix(exec, "-m icebath_cli.main desktop") || first != lde.executable() || strings.Contains(exec, interpreter) {
		t.Fatalf("exec %q", exec)
	}
}

func TestWrapperOwnershipRejectsSiblingExtensions(t *testing.T) {
	for _, suffix := range []string{"-old", ".bak", "-copy"} {
		tmp := t.TempDir()
		checkout := filepath.Join(tmp, "icebath-agent")
		_ = os.MkdirAll(checkout, 0o755)
		shim := ldeScript(t, filepath.Join(tmp, "shim"), "#!/usr/bin/env bash\nexec "+checkout+suffix+"/venv/bin/python "+checkout+suffix+"/icebath \"$@\"\n")
		if wrapperTargetsCheckout(shim, checkout) {
			t.Fatalf("%s accepted", suffix)
		}
	}
}

func TestWrapperOwnershipAcceptsShimViaSymlinkedHome(t *testing.T) {
	tmp, _ := ldeFixture(t)
	homeLink, homeReal := filepath.Join(tmp, "home-link"), filepath.Join(tmp, "home-real")
	_ = os.MkdirAll(filepath.Join(homeReal, "icebath-agent"), 0o755)
	if err := os.Symlink(homeReal, homeLink); err != nil {
		t.Skip("symlinks unavailable")
	}
	lexical := filepath.Join(homeLink, "icebath-agent")
	shim := ldeScript(t, filepath.Join(homeLink, ".local", "bin", "icebath"),
		"#!/usr/bin/env bash\nexec "+lexical+"/venv/bin/python "+lexical+"/icebath \"$@\"\n")
	if !wrapperTargetsCheckout(shim, lexical) {
		t.Fatal("lexical shim rejected")
	}
	t.Setenv("HOME", homeLink)
	repoScript := writeTestFile(t, filepath.Join(lexical, "icebath"), "#!/usr/bin/env python3\n", 0o644)
	ldeArgv0(repoScript)
	if got := resolveHermesBinForDesktopEntry(ldeResolveArgv0, lexical); got != shim {
		t.Fatalf("resolved %q want %q", got, shim)
	}
}

// Empty python interpreter in a venv bin dir set as the running executable
func ldeVenvInterpreter(t *testing.T, binDir string) string {
	interp := writeTestFile(t, filepath.Join(binDir, "python"), "", 0o644)
	lde.executable = func() string { return interp }
	return interp
}

func TestNeedsInterpreterCaseInsensitiveMatch(t *testing.T) {
	swapGUISteps(t)
	bin := filepath.Join(t.TempDir(), "MyEnv", "bin")
	interp := ldeVenvInterpreter(t, bin)
	script := writeTestFile(t, filepath.Join(bin, "icebath"), "#!"+interp+"\nimport icebath_cli\n", 0o644)
	if needsInterpreter(script) {
		t.Fatal("venv console script needs an interpreter")
	}
}

func TestNeedsInterpreterRejectsSiblingDirectory(t *testing.T) {
	swapGUISteps(t)
	tmp := t.TempDir()
	ldeVenvInterpreter(t, filepath.Join(tmp, "venv", "bin"))
	sibling := writeTestFile(t, filepath.Join(tmp, "sibling"), "#!"+tmp+"/venv/bin-extra/python\nimport icebath_cli\n", 0o644)
	if !needsInterpreter(sibling) {
		t.Fatal("sibling directory accepted")
	}
}

func TestNeedsInterpreterStripsFlagsBeforeComparing(t *testing.T) {
	swapGUISteps(t)
	tmp := t.TempDir()
	interp := ldeVenvInterpreter(t, filepath.Join(tmp, "venv", "bin"))
	flagged := writeTestFile(t, filepath.Join(tmp, "flagged"), "#!"+interp+" -S\nimport icebath_cli\n", 0o644)
	if needsInterpreter(flagged) {
		t.Fatal("flagged shebang needs an interpreter")
	}
}

func TestNeedsInterpreterEnvShebangAlwaysEscapes(t *testing.T) {
	swapGUISteps(t)
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "venv", "bin")
	interp := ldeVenvInterpreter(t, bin)
	envScript := writeTestFile(t, filepath.Join(tmp, "envscript"), "#!"+bin+"/env python3\nimport icebath_cli\n", 0o644)
	envAbs := writeTestFile(t, filepath.Join(tmp, "envabs"), "#!/usr/bin/env -S "+interp+"\nimport icebath_cli\n", 0o644)
	if !needsInterpreter(envScript) || needsInterpreter(envAbs) {
		t.Fatal("env shebang judged wrong")
	}
}

func TestProbeSkipsWrapperWithEscapingPythonShebang(t *testing.T) {
	tmp, xdg := ldeFixture(t)
	root := ldeProject(t, tmp)
	repoScript := ldeScript(t, filepath.Join(root, "icebath"), "#!/usr/bin/env python3\nimport icebath_cli\n")
	broken := ldeScript(t, filepath.Join(xdg, ".local", "bin", "icebath"), "#!/usr/bin/env python3\n# launcher for "+root+"\nimport icebath_cli\n")
	t.Setenv("HOME", xdg)
	ldeArgv0(repoScript)
	lde.resolveBin = ldeResolveArgv0
	exec := ldeValues(t, ldeInstall(t, root))["Exec"]
	if strings.Contains(exec, broken) || !strings.HasSuffix(exec, "-m icebath_cli.main desktop") {
		t.Fatalf("exec %q", exec)
	}
}

func TestProbeAcceptsShellLauncherWrapper(t *testing.T) {
	tmp, xdg := ldeFixture(t)
	root := ldeProject(t, tmp)
	repoScript := ldeScript(t, filepath.Join(root, "icebath"), "#!/usr/bin/env python3\nimport icebath_cli\n")
	good := ldeCheckoutWrapper(t, filepath.Join(xdg, ".local", "bin", "icebath"), root)
	t.Setenv("HOME", xdg)
	ldeArgv0(repoScript)
	lde.resolveBin = ldeResolveArgv0
	if exec := ldeValues(t, ldeInstall(t, root))["Exec"]; exec != good+" desktop" {
		t.Fatalf("exec %q", exec)
	}
}

func TestInstallIconHandlesTruncatedPngHeader(t *testing.T) {
	tmp, xdg := ldeFixture(t)
	root := ldeProject(t, tmp)
	writeTestFile(t, desktopIconPath(root), "\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR\x00\x00", 0o644)
	ldeFixedInstall(t, tmp)
	values := ldeValues(t, ldeInstall(t, root))
	if _, err := os.Stat(filepath.Join(xdg, "icons", "hicolor", "256x256", "apps", "icebath.png")); values["Icon"] != "icebath" || err != nil {
		t.Fatalf("values %v err %v", values, err)
	}
}

func TestHicolorSubdirPutsRastersInIndexedDirsNeverScalable(t *testing.T) {
	cases := []struct {
		w, h  int
		known bool
		want  string
	}{{0, 0, false, "256x256"}, {1024, 1024, true, "256x256"}, {512, 512, true, "512x512"}, {256, 256, true, "256x256"},
		{48, 48, true, "48x48"}, {24, 24, true, "24x24"}, {64, 32, true, "256x256"}}
	for _, c := range cases {
		if got := hicolorSubdir(c.w, c.h, c.known); got != c.want {
			t.Fatalf("%dx%d got %s want %s", c.w, c.h, got, c.want)
		}
	}
}

func TestInstallPlaces1024PngIn256x256NotScalable(t *testing.T) {
	tmp, xdg := ldeFixture(t)
	root := ldeProject(t, tmp)
	writeTestFile(t, desktopIconPath(root), pngIHDR(1024, 1024), 0o644)
	ldeFixedInstall(t, tmp)
	values := ldeValues(t, ldeInstall(t, root))
	got, _ := os.ReadFile(filepath.Join(xdg, "icons", "hicolor", "256x256", "apps", "icebath.png"))
	_, staleErr := os.Stat(filepath.Join(xdg, "icons", "hicolor", "scalable", "apps", "icebath.png"))
	if values["Icon"] != "icebath" || string(got) != pngIHDR(1024, 1024) || staleErr == nil {
		t.Fatalf("values %v", values)
	}
}

func TestInstallRemovesStaleScalablePng(t *testing.T) {
	tmp, xdg := ldeFixture(t)
	root := ldeProject(t, tmp)
	writeTestFile(t, desktopIconPath(root), pngIHDR(1024, 1024), 0o644)
	ldeFixedInstall(t, tmp)
	stale := writeTestFile(t, filepath.Join(xdg, "icons", "hicolor", "scalable", "apps", "icebath.png"), "old scalable png", 0o644)
	ldeInstall(t, root)
	_, destErr := os.Stat(filepath.Join(xdg, "icons", "hicolor", "256x256", "apps", "icebath.png"))
	if _, err := os.Stat(stale); err == nil || destErr != nil {
		t.Fatal("stale scalable icon kept or dest missing")
	}
}

func TestInstallExact48PngUses48x48Dir(t *testing.T) {
	tmp, xdg := ldeFixture(t)
	root := ldeProject(t, tmp)
	writeTestFile(t, desktopIconPath(root), pngIHDR(48, 48), 0o644)
	ldeFixedInstall(t, tmp)
	ldeInstall(t, root)
	_, destErr := os.Stat(filepath.Join(xdg, "icons", "hicolor", "48x48", "apps", "icebath.png"))
	_, staleErr := os.Stat(filepath.Join(xdg, "icons", "hicolor", "scalable", "apps", "icebath.png"))
	if destErr != nil || staleErr == nil {
		t.Fatal("48 px icon misplaced")
	}
}

func TestInstallResizesDecodablePngToPanelSizes(t *testing.T) {
	tmp, xdg := ldeFixture(t)
	root := ldeProject(t, tmp)
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for x := 0; x < 64; x++ {
		for y := 0; y < 64; y++ {
			c := color.NRGBA{255, 255, 255, 255}
			if x >= 16 && x < 48 && y >= 16 && y < 48 {
				c = color.NRGBA{0, 0, 0, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	writeTestFile(t, desktopIconPath(root), buf.String(), 0o644)
	ldeFixedInstall(t, tmp)
	ldeInstall(t, root)
	for size, sub := range map[uint32]string{24: "24x24", 256: "256x256"} {
		raw, err := os.ReadFile(filepath.Join(xdg, "icons", "hicolor", sub, "apps", "icebath.png"))
		if err != nil || binary.BigEndian.Uint32(raw[16:20]) != size || binary.BigEndian.Uint32(raw[20:24]) != size {
			t.Fatalf("%s err %v", sub, err)
		}
	}
	if _, err := os.Stat(filepath.Join(xdg, "icons", "hicolor", "scalable", "apps", "icebath.png")); err == nil {
		t.Fatal("scalable icon written")
	}
}

func TestDeferredInstallSkipsHealAfterExitWithoutReveal(t *testing.T) {
	var calls []string
	deferred, err := newDeferredDesktopEntryInstall(&bytes.Buffer{}, "/proj", func(root string) (string, error) {
		calls = append(calls, root)
		return "/entry", nil
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	deferred.start()
	time.Sleep(50 * time.Millisecond)
	if len(calls) != 0 {
		t.Fatal("installed while starting")
	}
	deferred.finish()
	select {
	case <-deferred.done:
	default:
		t.Fatal("waiter still running")
	}
	if len(calls) != 0 {
		t.Fatal("installed after exit without reveal")
	}
}

// Managed environment shaped tree whose launcher cannot serve the desktop
func ldeEnvShapedTree(t *testing.T, tmp string) string {
	tree := filepath.Join(tmp, "managed-env")
	writeTestFile(t, filepath.Join(tree, "workspace", "icebath_cli", "main.py"), "", 0o644)
	ldeScript(t, filepath.Join(tree, "venv", "bin", "icebath"), "#!/bin/sh\nexit 0\n")
	writeTestFile(t, filepath.Join(tree, "venv", "bin", "python"), "\x7fELF fake", 0o755)
	return tree
}

// Full install tree carrying the desktop app
func ldeFullTree(t *testing.T, tmp, name string) string {
	tree := filepath.Join(tmp, name)
	_ = os.MkdirAll(filepath.Join(tree, "icebath_cli"), 0o755)
	_ = os.MkdirAll(filepath.Join(tree, "apps", "desktop", "assets"), 0o755)
	writeTestFile(t, filepath.Join(tree, "apps", "desktop", "package.json"), "{}", 0o644)
	ldeScript(t, filepath.Join(tree, "venv", "bin", "icebath"), "#!/bin/sh\nexit 0\n")
	writeTestFile(t, filepath.Join(tree, "venv", "bin", "python"), "\x7fELF fake", 0o755)
	return tree
}

// Desktop capability as Python's None, True or False
func ldeCapability(candidate string) string {
	verdict, known := canServeDesktop(candidate, 2)
	if !known {
		return "None"
	}
	return pyBool(verdict)
}

func TestCapabilityRejectsEnvShapedLauncherAndAcceptsFullTree(t *testing.T) {
	tmp := t.TempDir()
	env, full := ldeEnvShapedTree(t, tmp), ldeFullTree(t, tmp, "install")
	got := []string{ldeCapability(filepath.Join(env, "venv/bin/icebath")), ldeCapability(filepath.Join(full, "venv/bin/icebath")),
		ldeCapability(filepath.Join(env, "venv/bin/python")), ldeCapability(filepath.Join(full, "venv/bin/python"))}
	if !slices.Equal(got, []string{"False", "True", "None", "None"}) {
		t.Fatalf("capabilities %v", got)
	}
}

func TestCapabilityUnknownShapesPass(t *testing.T) {
	tmp := t.TempDir()
	native := ldeScript(t, filepath.Join(tmp, "opt", "bin", "icebath"), "")
	if ldeCapability(filepath.Join(tmp, "nowhere", "bin", "icebath")) != "None" || ldeCapability(native) != "None" {
		t.Fatal("unknown shape judged")
	}
}

func TestCapabilityFollowsWrapperToItsTarget(t *testing.T) {
	tmp := t.TempDir()
	env, full := ldeEnvShapedTree(t, tmp), ldeFullTree(t, tmp, "install")
	bad := ldeScript(t, filepath.Join(tmp, "shims", "icebath"), "#!/usr/bin/env bash\nexec \""+env+"/venv/bin/icebath\" \"$@\"\n")
	good := ldeScript(t, filepath.Join(tmp, "shims", "icebath-good"), "#!/usr/bin/env bash\nexec \""+full+"/venv/bin/icebath\" \"$@\"\n")
	if ldeCapability(bad) != "False" || ldeCapability(good) != "True" {
		t.Fatal("wrapper target not followed")
	}
}

func TestResolverSkipsIncapablePrimaryForWrapper(t *testing.T) {
	tmp, _ := ldeFixture(t)
	env := ldeEnvShapedTree(t, tmp)
	checkout := ldeFullTree(t, tmp, "checkout")
	wrapper := ldeScript(t, filepath.Join(tmp, ".local", "bin", "icebath"), "#!/usr/bin/env bash\nexec \""+checkout+"/venv/bin/icebath\" \"$@\"\n")
	if got := resolveHermesBinForDesktopEntry(ldeResolve(filepath.Join(env, "venv", "bin", "icebath")), checkout); got != wrapper {
		t.Fatalf("resolved %q", got)
	}
}

func TestInstallSkipsWriteWhenExecProvablyCannotServeDesktop(t *testing.T) {
	tmp, _ := ldeFixture(t)
	root := ldeProject(t, tmp)
	env := ldeEnvShapedTree(t, tmp)
	lde.managementEnabled = func() bool { return true }
	lde.resolveExecCommand = func(string) string { return filepath.Join(env, "venv", "bin", "icebath") + " desktop" }
	if entry := ldeInstall(t, root); entry != "" {
		t.Fatalf("entry %s", entry)
	}
	if _, err := os.Stat(desktopEntryPath()); err == nil {
		t.Fatal("entry written")
	}
}

func TestInstallLeavesExistingEntryUntouchedWhenExecIncapable(t *testing.T) {
	tmp, _ := ldeFixture(t)
	root := ldeProject(t, tmp)
	env := ldeEnvShapedTree(t, tmp)
	entryPath := writeTestFile(t, desktopEntryPath(), "hand-tuned\n", 0o644)
	lde.managementEnabled = func() bool { return true }
	lde.resolveExecCommand = func(string) string { return filepath.Join(env, "venv", "bin", "icebath") + " desktop" }
	if entry := ldeInstall(t, root); entry != "" {
		t.Fatalf("entry %s", entry)
	}
	if raw, _ := os.ReadFile(entryPath); string(raw) != "hand-tuned\n" {
		t.Fatalf("entry %q", raw)
	}
}

func TestModuleFormPassesTheGate(t *testing.T) {
	env := ldeEnvShapedTree(t, t.TempDir())
	if _, known := persistedExecServesDesktop("\"" + filepath.Join(env, "venv", "bin", "python") + "\" -m icebath_cli.main desktop"); known {
		t.Fatal("module form judged")
	}
}

func TestInstallThroughWrapperWhenPrimaryIsIncapable(t *testing.T) {
	tmp, _ := ldeFixture(t)
	root := ldeFullTree(t, tmp, "checkout")
	writeTestFile(t, filepath.Join(root, "apps", "desktop", "assets", "icon.png"), "\x89PNG fake", 0o644)
	env := ldeEnvShapedTree(t, tmp)
	wrapper := ldeScript(t, filepath.Join(tmp, ".local", "bin", "icebath"), "#!/usr/bin/env bash\nexec \""+root+"/venv/bin/icebath\" \"$@\"\n")
	lde.managementEnabled = func() bool { return true }
	lde.resolveBin = ldeResolve(filepath.Join(env, "venv", "bin", "icebath"))
	entry := ldeInstall(t, root)
	if entry != desktopEntryPath() || ldeValues(t, entry)["Exec"] != wrapper+" desktop" {
		t.Fatalf("entry %s values %v", entry, ldeValues(t, entry))
	}
}

func TestExecAppendsSkipBuildWhenPackagedAppExists(t *testing.T) {
	tmp, _ := ldeFixture(t)
	root := ldeProject(t, tmp)
	writeTestFile(t, filepath.Join(root, "apps", "desktop", "release", "linux-unpacked", "icebath"), "", 0o644)
	lde.resolveBin = ldeResolve("")
	if exec := ldeValues(t, ldeInstall(t, root))["Exec"]; !strings.HasSuffix(exec, "desktop --skip-build") {
		t.Fatalf("exec %q", exec)
	}
}

func TestExecKeepsBuildThenLaunchWhenNoPackagedApp(t *testing.T) {
	tmp, _ := ldeFixture(t)
	lde.resolveBin = ldeResolve("")
	if exec := ldeValues(t, ldeInstall(t, ldeProject(t, tmp)))["Exec"]; !strings.HasSuffix(exec, "desktop") || strings.Contains(exec, "--skip-build") {
		t.Fatalf("exec %q", exec)
	}
}
