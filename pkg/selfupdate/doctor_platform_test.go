package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Doctor print matcher for TLS rows that keys on words instead of spacing
func tlsOutNormalized(out string) string {
	return strings.Join(strings.Fields(strings.ToLower(out)), " ")
}

func TestCheckCertificatesExercisesRealTlsPolicy(t *testing.T) {
	d, out := toolsDoctor(t)
	toolsSeam(t, &doctorInstallTrustStore, func() bool { return true })
	issues := []string{}
	if err := doctorCheckCertificatesRows(context.Background(), d, false, &issues); err != nil {
		t.Fatal(err)
	}
	text := tlsOutNormalized(out.String())
	if !strings.Contains(text, "✓") || !strings.Contains(text, "tls") || strings.Contains(text, "skipped") || strings.Contains(text, "verification active") {
		t.Fatalf("output: %s", text)
	}
	if !strings.Contains(text, "platform trust") && !strings.Contains(text, "configured") {
		t.Fatalf("output: %s", text)
	}
}

func TestCheckCertificatesFallbackNamesOpensslWithoutPlatformTrustClaim(t *testing.T) {
	d, out := toolsDoctor(t)
	toolsSeam(t, &doctorInstallTrustStore, func() bool { return false })
	issues := []string{}
	if err := doctorCheckCertificatesRows(context.Background(), d, false, &issues); err != nil {
		t.Fatal(err)
	}
	raw := out.String()
	text := tlsOutNormalized(raw)
	if !strings.Contains(raw, "⚠") || !strings.Contains(text, "trust store") || !strings.Contains(text, "openssl") ||
		strings.Contains(text, "verification active") || !strings.Contains(text, "available") {
		t.Fatalf("output: %s", raw)
	}
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "✓") && strings.Contains(line, "platform trust") {
			t.Fatalf("platform trust claimed: %s", line)
		}
	}
}

func TestDoctorPlatformHintsSqliteUpgradeHintRecreatesDockerContainers(t *testing.T) {
	d, _ := toolsDoctor(t)
	toolsSeam(t, &doctorDetectInstallMethod, func(context.Context, string, string) (string, error) { return "docker", nil })
	hint, err := doctorSQLiteUpgradeHint(context.Background(), d, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hint, "docker pull ghcr.io/nickheyer/icebath:latest") || strings.Contains(hint, "icebath update") {
		t.Fatalf("hint = %q", hint)
	}
}

func TestDoctorPlatformHintsSqliteUpgradeHintAptStampNamesTermuxExternalUpdate(t *testing.T) {
	d, _ := toolsDoctor(t)
	hint, err := doctorSQLiteUpgradeHint(context.Background(), d, "apt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hint, "run `pkg upgrade icebath-agent`") || strings.Contains(hint, "icebath update") {
		t.Fatalf("hint = %q", hint)
	}
}

func TestDoctorPlatformHintsSqliteUpgradeHintPackageManagerStampNamesItsUpgrade(t *testing.T) {
	d, _ := toolsDoctor(t)
	for method, want := range map[string]string{"homebrew": "run `brew upgrade --cask icebath`", "chocolatey": "run `choco upgrade icebath`",
		"pip": "run `pipx upgrade icebath-cli`", "dpkg": "run `sudo apt update && sudo apt install --only-upgrade icebath`"} {
		hint, err := doctorSQLiteUpgradeHint(context.Background(), d, method)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(hint, want) || strings.Contains(hint, "icebath update") {
			t.Fatalf("%s hint = %q", method, hint)
		}
	}
	for _, method := range []string{"script", "setup-app"} {
		if hint, err := doctorSQLiteUpgradeHint(context.Background(), d, method); err != nil || !strings.Contains(hint, "run `icebath update`") {
			t.Fatalf("%s hint = %q %v", method, hint, err)
		}
	}
}

func TestDoctorPlatformHintsSqliteUpgradeHintPreservesNixGuidanceAsProse(t *testing.T) {
	d, _ := toolsDoctor(t)
	guidance := config.RecommendedUpdateCommandForMethod("nix")
	hint, err := doctorSQLiteUpgradeHint(context.Background(), d, "nix")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hint, guidance) || strings.Contains(hint, "run `"+guidance+"`") || strings.Contains(hint, "icebath update") {
		t.Fatalf("hint = %q", hint)
	}
}

func TestStagedRuntimeVenvResolvedPathWithoutVenvMarkerIsNotStaged(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "venv")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	toolsSeam(t, &doctorSelectedVenvDir, func(context.Context) (string, error) { return empty, nil })
	if staged := doctorStagedVenvDir(context.Background()); staged != "" {
		t.Fatalf("staged = %q", staged)
	}
}

func TestStagedRuntimeVenvUnreadablePmDegradesToNone(t *testing.T) {
	toolsSeam(t, &doctorSelectedVenvDir, func(context.Context) (string, error) { return "", errors.New("pm unavailable") })
	if staged := doctorStagedVenvDir(context.Background()); staged != "" {
		t.Fatalf("staged = %q", staged)
	}
}

func TestStagedRuntimeVenvNothingStagedKeepsLegacyInterpreterProbe(t *testing.T) {
	d, out := toolsDoctor(t)
	toolsSeam(t, &doctorStagedVenvDir, func(context.Context) string { return "" })
	toolsSeam(t, &doctorRuntimePython, func(context.Context) (*doctorPython, error) {
		return &doctorPython{Version: [3]int{3, 11, 9}, Prefix: "/usr", BasePrefix: "/usr"}, nil
	})
	if _, err := checkPythonEnvironment(context.Background(), d, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Runtime venv staged") || !strings.Contains(out.String(), "Not in virtual environment") {
		t.Fatalf("output:\n%s", out.String())
	}
}

// Stands a desktop bundle on a macOS host, with a canned designated requirement unless real is set
func darwinBundle(t *testing.T, dr string, readable, real bool) (*Doctor, *bytes.Buffer) {
	t.Helper()
	d, out := toolsDoctor(t)
	toolsSeam(t, &doctorSysPlatform, "darwin")
	app := filepath.Join(t.TempDir(), "Icebath.app")
	toolsSeam(t, &doctorDesktopAppBundle, func(*Doctor) string { return app })
	if !real {
		toolsSeam(t, &doctorMacosDesktopDR, func(context.Context, string) (string, bool) { return dr, readable })
	}
	return d, out
}

func TestMacOSTCCGrantsSilentWithoutDesktopBundle(t *testing.T) {
	d, out := toolsDoctor(t)
	toolsSeam(t, &doctorSysPlatform, "darwin")
	toolsSeam(t, &doctorDesktopAppBundle, func(*Doctor) string { return "" })
	doctorCheckMacosTCCGrants(context.Background(), d)
	if out.String() != "" {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestMacOSTCCGrantsWarnsOnCdhashPinnedDr(t *testing.T) {
	d, out := darwinBundle(t, `designated => identifier "ai.icebath" and cdhash H"97e692f3890f781fa0ad5ad6cb9d769cfaf42628"`, true, false)
	doctorCheckMacosTCCGrants(context.Background(), d)
	text := out.String()
	if !strings.Contains(text, "TCC grants will reset after every update") || !strings.Contains(text, "icebath update") || strings.Contains(text, "signing identity is stable") {
		t.Fatalf("output:\n%s", text)
	}
}

func TestMacOSTCCGrantsIdentifierDrIsStableWithUpgradeHintAndRepairInfo(t *testing.T) {
	d, out := darwinBundle(t, `designated => identifier "ai.icebath"`, true, false)
	doctorCheckMacosTCCGrants(context.Background(), d)
	text := out.String()
	if !strings.Contains(text, "TCC signing identity is stable") || !strings.Contains(text, "--setup-tcc-identity") ||
		!strings.Contains(text, "tccutil reset ScreenCapture ai.icebath") {
		t.Fatalf("output:\n%s", text)
	}
}

func TestMacOSTCCGrantsCertificateAnchoredDrIsStableWithoutUpgradeHint(t *testing.T) {
	d, out := darwinBundle(t, `designated => identifier "ai.icebath" and certificate root = H"aabbcc"`, true, false)
	doctorCheckMacosTCCGrants(context.Background(), d)
	text := out.String()
	if !strings.Contains(text, "TCC signing identity is stable") || strings.Contains(text, "--setup-tcc-identity") ||
		!strings.Contains(text, "tccutil reset ScreenCapture ai.icebath") {
		t.Fatalf("output:\n%s", text)
	}
}

func TestMacOSTCCGrantsUnreadableDrWarnsAndNeverClaimsStable(t *testing.T) {
	for _, failure := range []string{"none", "empty", "timeout", "no_codesign"} {
		t.Run(failure, func(t *testing.T) {
			var d *Doctor
			var out *bytes.Buffer
			switch failure {
			case "none":
				d, out = darwinBundle(t, "", false, false)
			case "empty":
				d, out = darwinBundle(t, "", true, false)
			case "timeout":
				d, out = darwinBundle(t, "", false, true)
				toolsSeam(t, &doctorWhich, func(string) string { return "/usr/bin/codesign" })
				toolsSeam(t, &doctorSubprocessRun, func(context.Context, []string, pysubprocess.Options) (*pysubprocess.Completed, error) {
					return nil, &pysubprocess.TimeoutExpired{}
				})
			default:
				d, out = darwinBundle(t, "", false, true)
				toolsSeam(t, &doctorWhich, func(string) string { return "" })
			}
			doctorCheckMacosTCCGrants(context.Background(), d)
			text := out.String()
			if !strings.Contains(text, "could not read code-signing requirement") || strings.Contains(text, "stable") {
				t.Fatalf("output:\n%s", text)
			}
		})
	}
}

// Source tree, data home and command link location of one command installation case
type commandTree struct {
	d       *Doctor
	out     *bytes.Buffer
	project string
	home    string
	command string
}

// Builds a git stamped source tree with its own data home and an empty command dir on PATH
func commandInstallTree(t *testing.T) commandTree {
	t.Helper()
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	dataHome, project := filepath.Join(root, "data"), filepath.Join(root, "source")
	for _, dir := range []string{dataHome, project} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(project, ".install_method"), "git", 0o644)
	t.Setenv("ICEBATH_HOME", dataHome)
	t.Setenv("ICEBATH_RUNTIME_DIR", filepath.Join(dataHome, "tools"))
	t.Setenv("PREFIX", "")
	os.Unsetenv("PREFIX")
	t.Setenv("HOME", root)
	toolsSeam(t, &doctorUserHomeDir, func() (string, error) { return root, nil })
	command := filepath.Join(root, ".local", "bin", "icebath")
	if err := os.MkdirAll(filepath.Dir(command), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(command))
	out := &bytes.Buffer{}
	return commandTree{&Doctor{W: out, Home: dataHome, ProjectRoot: project, DisplayHome: dataHome}, out, project, dataHome, command}
}

// Writes a file with its parents, failing the test on error
func writeTestFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// Records a selected dependency generation for a project the way a committed sync does
func commandGeneration(t *testing.T, project string) string {
	t.Helper()
	stateDir := pm.InstallStateDir(context.Background(), project)
	selected := filepath.Join(stateDir, "environments", "selected", "venv")
	writeTestFile(t, filepath.Join(selected, "pyvenv.cfg"), "home = fixture\nversion = 3.11.9\n", 0o644)
	site, err := pm.SitePackages(selected)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	facts, _ := json.Marshal(map[string]any{"packages": map[string]any{"venv": map[string]any{"environment": selected}}})
	writeTestFile(t, filepath.Join(stateDir, "facts.json"), string(facts), 0o644)
	return selected
}

// Gives a project its install binary and a tool store that records an interpreter
func commandPMSource(t *testing.T, project, dataHome string) {
	t.Helper()
	writeTestFile(t, filepath.Join(project, "icebath"), "#!/bin/sh\necho selected\n", 0o755)
	interpreter := filepath.Join(dataHome, "tools", "python-fixture", "bin", "python3")
	if err := os.MkdirAll(filepath.Dir(interpreter), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/sh", interpreter); err != nil {
		t.Fatal(err)
	}
	facts, _ := json.Marshal(map[string]any{"packages": map[string]any{"python": map[string]any{"entry": "python-fixture"}}})
	writeTestFile(t, filepath.Join(dataHome, "tools", "facts.json"), string(facts), 0o644)
}

// Reports whether a path is a symbolic link
func isTestSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

func TestPmGenerationDoesNotRequireALegacyConsoleScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix only")
	}
	tree := commandInstallTree(t)
	selected := commandGeneration(t, tree.project)
	commandPMSource(t, tree.project, tree.home)
	staged, err := pm.StageLauncher(context.Background(), "icebath", tree.project, filepath.Dir(tree.command))
	if err != nil || staged != tree.command {
		t.Fatalf("staged = %q, err = %v", staged, err)
	}
	f, err := checkCommandInstallation(context.Background(), tree.d, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Issues) != 0 || len(f.ManualIssues) != 0 {
		t.Fatalf("finding = %+v\n%s", f, tree.out.String())
	}
	if _, err := os.Lstat(filepath.Join(tree.project, "venv")); err == nil {
		t.Fatal("legacy venv created")
	}
	if _, err := os.Lstat(filepath.Join(selected, "bin", "icebath")); err == nil {
		t.Fatal("console script created in the generation")
	}
	if info, err := os.Lstat(tree.command); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("command is not a regular file: %v", err)
	}
}

func TestPmFixPublishesAGenerationAwareLauncher(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix only")
	}
	for _, prior := range []string{"missing", "legacy"} {
		t.Run(prior, func(t *testing.T) {
			tree := commandInstallTree(t)
			commandPMSource(t, tree.project, tree.home)
			commandGeneration(t, tree.project)
			stale := filepath.Join(tree.project, "venv", "bin", "icebath")
			writeTestFile(t, stale, "#!/bin/sh\nexit 99\n", 0o755)
			if prior == "legacy" {
				if err := os.Symlink(stale, tree.command); err != nil {
					t.Fatal(err)
				}
			}
			f, err := checkCommandInstallation(context.Background(), tree.d, true)
			if err != nil {
				t.Fatal(err)
			}
			if f.Fixed != 1 {
				t.Fatalf("fixed = %d\n%s", f.Fixed, tree.out.String())
			}
			if isTestSymlink(tree.command) {
				t.Fatal("command is still a symlink")
			}
			cmd := exec.Command(tree.command)
			cmd.Dir = filepath.Dir(tree.project)
			cmd.Env = append(os.Environ(), "PATH=/usr/bin:/bin")
			stdout, err := cmd.Output()
			if err != nil || strings.TrimSpace(string(stdout)) != "selected" {
				t.Fatalf("stdout = %q, err = %v", stdout, err)
			}
			if info, err := os.Stat(stale); err != nil || !info.Mode().IsRegular() {
				t.Fatalf("stale entry removed: %v", err)
			}
		})
	}
}

func TestFixPreservesUserManagedCommands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix only")
	}
	for _, kind := range []string{"wrapper", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			tree := commandInstallTree(t)
			commandPMSource(t, tree.project, tree.home)
			commandGeneration(t, tree.project)
			wrapper := filepath.Join(filepath.Dir(tree.project), "custom-wrapper")
			body := "#!/bin/sh\nexit 42\n"
			writeTestFile(t, wrapper, body, 0o755)
			if kind == "symlink" {
				if err := os.Symlink(wrapper, tree.command); err != nil {
					t.Fatal(err)
				}
			} else {
				writeTestFile(t, tree.command, body, 0o755)
			}
			f, err := checkCommandInstallation(context.Background(), tree.d, true)
			if err != nil {
				t.Fatal(err)
			}
			if f.Fixed != 0 {
				t.Fatalf("fixed = %d\n%s", f.Fixed, tree.out.String())
			}
			if raw, err := os.ReadFile(tree.command); err != nil || string(raw) != body {
				t.Fatalf("command body = %q, err = %v", raw, err)
			}
			if kind == "symlink" {
				resolved, err := filepath.EvalSymlinks(tree.command)
				if !isTestSymlink(tree.command) || err != nil || resolved != wrapper {
					t.Fatalf("command no longer links the wrapper: %q %v", resolved, err)
				}
				if !strings.Contains(strings.ToLower(strings.Join(f.ManualIssues, "\n")), "manual") {
					t.Fatalf("manual issues = %q", f.ManualIssues)
				}
			}
		})
	}
}

func TestDoctorReportsSelectedImportTreeNotInterpreterPrefix(t *testing.T) {
	for _, layout := range []string{"legacy", "generation"} {
		for _, active := range []bool{true, false} {
			name := layout + "-outside"
			if active {
				name = layout + "-active"
			}
			t.Run(name, func(t *testing.T) {
				tree := commandInstallTree(t)
				var selected string
				if layout == "generation" {
					selected = commandGeneration(t, tree.project)
				} else {
					selected = filepath.Join(tree.project, "venv")
					writeTestFile(t, filepath.Join(selected, "pyvenv.cfg"), "home = fixture\nversion = 3.11.9\n", 0o644)
				}
				site, err := pm.SitePackages(selected)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(site, 0o755); err != nil {
					t.Fatal(err)
				}
				toolsSeam(t, &doctorSelectedVenvDir, func(ctx context.Context) (string, error) { return pm.SelectedVenv(ctx, tree.project) })
				toolsSeam(t, &doctorRuntimePython, func(context.Context) (*doctorPython, error) {
					py := &doctorPython{Version: [3]int{3, 11, 9}, Prefix: selected, BasePrefix: "/usr", Path: []string{"/usr/lib/python3.11"}}
					if active {
						py.Prefix = "/usr"
						py.Path = append([]string{site}, py.Path...)
					}
					return py, nil
				})
				if _, err := checkPythonEnvironment(context.Background(), tree.d, false); err != nil {
					t.Fatal(err)
				}
				text := tree.out.String()
				if !strings.Contains(text, selected) {
					t.Fatalf("selected tree is not named:\n%s", text)
				}
				if strings.Contains(text, "active in this process") != active || strings.Contains(text, "runs outside it") == active {
					t.Fatalf("output:\n%s", text)
				}
			})
		}
	}
}

func TestRemediesAndLauncherRepairsRespectInstallOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix only")
	}
	for _, tc := range []struct{ method, remedy string }{
		{"git", "icebath pm repair"}, {"nix", "Nix"}, {"docker", "docker pull"}, {"apt", "pkg upgrade"},
		{"script", "icebath pm repair"}, {"setup-app", "icebath pm repair"}, {"chocolatey", "choco upgrade icebath"},
		{"pip", "pipx upgrade icebath-cli"}, {"homebrew", "brew upgrade --cask icebath"}, {"scoop", "scoop update icebath"},
		{"winget", "winget upgrade --id nickheyer.icebath"}, {"dpkg", "sudo apt install --only-upgrade icebath"},
		{"rpm", "sudo dnf upgrade icebath"}, {"pacman", "paru -Syu icebath-bin"}, {"apk", "apk upgrade icebath"},
	} {
		t.Run(tc.method, func(t *testing.T) {
			tree := commandInstallTree(t)
			writeTestFile(t, filepath.Join(tree.project, ".install_method"), tc.method, 0o644)
			entry := filepath.Join(tree.project, "venv", "bin", "icebath")
			writeTestFile(t, entry, "#!/bin/sh\nexit 0\n", 0o755)
			toolsSeam(t, &doctorStagedVenvDir, func(context.Context) string { return "" })
			toolsSeam(t, &doctorRuntimePython, func(context.Context) (*doctorPython, error) { return nil, errors.New("no interpreter") })
			findings := &Finding{}
			for _, check := range []doctorCheckFunc{checkPythonEnvironment, checkCommandInstallation} {
				f, err := check(context.Background(), tree.d, true)
				if err != nil {
					t.Fatal(err)
				}
				findings.Merge(f)
			}
			text := tree.out.String() + "\n" + strings.Join(append(findings.Issues, findings.ManualIssues...), "\n")
			if !strings.Contains(text, tc.remedy) || strings.Contains(text, "pip install") {
				t.Fatalf("output:\n%s", text)
			}
			if tc.method == "git" || tc.method == "script" || tc.method == "setup-app" {
				resolved, err := filepath.EvalSymlinks(tree.command)
				if !isTestSymlink(tree.command) || err != nil || resolved != entry {
					t.Fatalf("command does not link the entry: %q %v\n%s", resolved, err, text)
				}
				return
			}
			if _, err := os.Lstat(tree.command); err == nil {
				t.Fatal("command created for an externally owned install")
			}
			if strings.Contains(text, "icebath pm repair") {
				t.Fatalf("output:\n%s", text)
			}
		})
	}
}

func TestCheckPythonEnvironmentReportsAMissingInterpreterWithTheRepairHint(t *testing.T) {
	d, out := toolsDoctor(t)
	toolsSeam(t, &doctorStagedVenvDir, func(context.Context) string { return "" })
	toolsSeam(t, &doctorDetectInstallMethod, func(context.Context, string, string) (string, error) { return "git", nil })
	toolsSeam(t, &doctorRuntimePython, func(context.Context) (*doctorPython, error) { return nil, errors.New("no interpreter") })
	f, err := checkPythonEnvironment(context.Background(), d, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Issues) != 1 || !strings.Contains(f.Issues[0], "icebath pm repair") {
		t.Fatalf("issues = %q", f.Issues)
	}
	if !strings.Contains(out.String(), "✗ Python (missing)") || !strings.Contains(out.String(), "SQLite ") || !strings.Contains(out.String(), "Not in virtual environment") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestCheckPythonEnvironmentGradesTheInterpreterVersion(t *testing.T) {
	for _, tc := range []struct {
		version [3]int
		want    string
		issues  int
	}{
		{[3]int{3, 7, 9}, "✗ Python 3.7.9 (3.10+ required)", 1},
		{[3]int{3, 9, 1}, "⚠ Python 3.9.1 (3.10+ recommended)", 0},
		{[3]int{3, 10, 4}, "Python 3.11+ recommended for RL Training tools", 0},
		{[3]int{3, 12, 0}, "✓ Python 3.12.0", 0},
	} {
		d, out := toolsDoctor(t)
		toolsSeam(t, &doctorStagedVenvDir, func(context.Context) string { return "" })
		toolsSeam(t, &doctorRuntimePython, func(context.Context) (*doctorPython, error) {
			return &doctorPython{Version: tc.version, Prefix: "/venv", BasePrefix: "/usr"}, nil
		})
		f, err := checkPythonEnvironment(context.Background(), d, false)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), tc.want) || len(f.Issues) != tc.issues || !strings.Contains(out.String(), "Virtual environment active") {
			t.Fatalf("version %v: issues = %q, output:\n%s", tc.version, f.Issues, out.String())
		}
	}
}

// Runs the dashboard load check against a canned child result, recording the child invocations
func runDashboardCheck(t *testing.T, returncode int, stderr string, recorded *[]dashboardProbeCall) (*Finding, string) {
	t.Helper()
	d, out := toolsDoctor(t)
	toolsSeam(t, &doctorDashboardProbeArgv, func(context.Context, string) ([]string, error) {
		return []string{"/opt/icebath/icebath", "dashboard-load-probe"}, nil
	})
	toolsSeam(t, &doctorSubprocessRun, func(_ context.Context, argv []string, opts pysubprocess.Options) (*pysubprocess.Completed, error) {
		*recorded = append(*recorded, dashboardProbeCall{argv, opts})
		return &pysubprocess.Completed{Args: argv, Returncode: returncode, Stderr: []byte(stderr)}, nil
	})
	f, err := checkWebDashboardImport(context.Background(), d, false)
	if err != nil {
		t.Fatal(err)
	}
	return f, out.String()
}

// One recorded child invocation of the dashboard load check
type dashboardProbeCall struct {
	argv []string
	opts pysubprocess.Options
}

func TestDriftedPairIsAReportedIssue(t *testing.T) {
	stderr := "File \".../fastapi/routing.py\", line 835, in __init__\n" +
		"TypeError: Router.__init__() got an unexpected keyword argument 'on_startup'\n"
	var recorded []dashboardProbeCall
	finding, out := runDashboardCheck(t, 1, stderr, &recorded)
	if !strings.Contains(out, "Dashboard web surface") || !strings.Contains(out, "✗") ||
		!strings.Contains(out, "Router.__init__() got an unexpected keyword argument 'on_startup'") {
		t.Fatalf("output:\n%s", out)
	}
	found := false
	for _, issue := range finding.Issues {
		found = found || strings.Contains(issue, "icebath pm repair")
	}
	if !found {
		t.Fatalf("issues = %q", finding.Issues)
	}
}

func TestProbeRunsWithoutLazyInstalls(t *testing.T) {
	var recorded []dashboardProbeCall
	t.Setenv(pm.EnvDisableLazyInstalls, "0")
	runDashboardCheck(t, 0, "", &recorded)
	if len(recorded) != 1 {
		t.Fatalf("child ran %d times", len(recorded))
	}
	call := recorded[0]
	if strings.Join(call.argv, " ") != "/opt/icebath/icebath dashboard-load-probe" {
		t.Fatalf("argv = %q", call.argv)
	}
	lazy := []string{}
	for _, entry := range call.opts.Env {
		if strings.HasPrefix(entry, pm.EnvDisableLazyInstalls+"=") {
			lazy = append(lazy, entry)
		}
	}
	if len(lazy) != 1 || lazy[0] != pm.EnvDisableLazyInstalls+"=1" {
		t.Fatalf("lazy install env = %q", lazy)
	}
	if call.opts.Timeout.Seconds() < 60 {
		t.Fatalf("timeout = %v", call.opts.Timeout)
	}
}

func TestRequiredPackagesListsOptionalCapabilitiesWithHermesRows(t *testing.T) {
	d, out := toolsDoctor(t)
	toolsSeam(t, &doctorPackages, []doctorPackage{
		{"Loaded library", func(context.Context) bool { return true }},
		{"Absent library", func(context.Context) bool { return false }},
	})
	f, err := checkRequiredPackages(context.Background(), d, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Issues) != 0 || len(f.ManualIssues) != 0 {
		t.Fatalf("issues = %q %q", f.Issues, f.ManualIssues)
	}
	text := out.String()
	if !strings.Contains(text, "✓ Loaded library (optional)") || !strings.Contains(text, "⚠ Absent library (optional, not installed)") {
		t.Fatalf("output:\n%s", text)
	}
}

func TestRequiredPackagesNamesNoPythonLibraryCompiledIntoTheBinary(t *testing.T) {
	for _, p := range doctorPackages {
		for _, compiled := range []string{"OpenAI SDK", "Rich", "python-dotenv", "ruamel.yaml", "HTTPX", "Croniter", "python-telegram-bot", "discord.py"} {
			if strings.Contains(p.Name, compiled) {
				t.Fatalf("row %q names %q, which the binary carries", p.Name, compiled)
			}
		}
	}
}

func TestRequiredPackagesProbesTheRuntimeEnvironmentForTheBrowserHarness(t *testing.T) {
	d, out := toolsDoctor(t)
	var asked []string
	toolsSeam(t, &doctorModuleImportable, func(_ context.Context, module string) bool {
		asked = append(asked, module)
		return false
	})
	var harness []doctorPackage
	for _, p := range doctorPackages {
		if strings.HasPrefix(p.Name, "browser-harness") {
			harness = append(harness, p)
		}
	}
	toolsSeam(t, &doctorPackages, harness)
	if _, err := checkRequiredPackages(context.Background(), d, false); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != "browser_harness" || !strings.Contains(out.String(), "⚠ browser-harness (browser_exec) (optional, not installed)") {
		t.Fatalf("asked %q, output:\n%s", asked, out.String())
	}
}
